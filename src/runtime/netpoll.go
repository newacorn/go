// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || (js && wasm) || windows

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// Integrated network poller (platform-independent part).
// A particular implementation (epoll/kqueue/port/AIX/Windows)
// must define the following functions:
//
// func netpollinit()
//     Initialize the poller. Only called once.
//
// func netpollopen(fd uintptr, pd *pollDesc) int32
//     Arm edge-triggered notifications for fd. The pd argument is to pass
//     back to netpollready when fd is ready. Return an errno value.
//
// func netpollclose(fd uintptr) int32
//     Disable notifications for fd. Return an errno value.
//
// func netpoll(delta int64) gList
//     Poll the network. If delta < 0, block indefinitely. If delta == 0,
//     poll without blocking. If delta > 0, block for up to delta nanoseconds.
//     Return a list of goroutines built by calling netpollready.
//
// func netpollBreak()
//     Wake up the network poller, assumed to be blocked in netpoll.
//
// func netpollIsPollDescriptor(fd uintptr) bool
//     Reports whether fd is a file descriptor used by the poller.

// Error codes returned by runtime_pollReset and runtime_pollWait.
// These must match the values in internal/poll/fd_poll_runtime.go.
const (
	pollNoError        = 0 // no error
	pollErrClosing     = 1 // descriptor is closed
	pollErrTimeout     = 2 // I/O timeout
	// 目前只与读操作相关， pollDesc.eventErr() 返回true会认为是此错误。
	pollErrNotPollable = 3 // general error polling descriptor
)

// pollDesc contains 2 binary semaphores, rg and wg, to park reader and writer
// goroutines respectively. The semaphore can be in the following states:
//
//	pdReady - io readiness notification is pending;
//	          a goroutine consumes the notification by changing the state to pdNil.
//	pdWait - a goroutine prepares to park on the semaphore, but not yet parked;
//	         the goroutine commits to park by changing the state to G pointer,
//	         or, alternatively, concurrent io notification changes the state to pdReady,
//	         or, alternatively, concurrent timeout/close changes the state to pdNil.
//	G pointer - the goroutine is blocked on the semaphore;
//	            io notification or timeout/close changes the state to pdReady or pdNil respectively
//	            and unparks the goroutine.
//	pdNil - none of the above.
const (
	pdNil   uintptr = 0
	pdReady uintptr = 1
	pdWait  uintptr = 2
)

const pollBlockSize = 4 * 1024

// Network poller descriptor.
//
// No heap pointers.
type pollDesc struct {
	_    sys.NotInHeap
	// 实现 pollCache 缓存，将空闲的 pollDesc 串成一个链表。
	link *pollDesc // in pollcache, protected by pollcache.lock
	// 要监听的文件描述符。
	fd   uintptr   // constant for pollDesc usage lifetime

	// atomicInfo holds bits from closing, rd, and wd,
	// which are only ever written while holding the lock,
	// summarized for use by netpollcheckerr,
	// which cannot acquire the lock.
	// After writing these fields under lock in a way that
	// might change the summary, code must call publishInfo
	// before releasing the lock.
	// Code that changes fields and then calls netpollunblock
	// (while still holding the lock) must call publishInfo
	// before calling netpollunblock, because publishInfo is what
	// stops netpollblock from blocking anew
	// (by changing the result of netpollcheckerr).
	// atomicInfo also holds the eventErr bit,
	// recording whether a poll event on the fd got an error;
	// atomicInfo is the only source of truth for that bit.
	atomicInfo atomic.Uint32 // atomic pollInfo

	// rg, wg are accessed atomically and hold g pointers.
	// (Using atomic.Uintptr here is similar to using guintptr elsewhere.)

	// rg, wg are accessed atomically and hold g pointers.
	// (Using atomic.Uintptr here is similar to using guintptr elsewhere.)
	//
	// 有4种可能的值，常量 pdReady 、 pdWait ，一个G的指针以及nil。
	// pdReady 【状态转移：*g -> pdReady（由 netpollunblock 负责
	// 状态变更） -> 0（由恢复的协程负责状态变更） 】表示fd 的数据已经
	// 准备就绪(但还未被所关联的g所读)（在 netpollunblock 函数中,在fd
	// 有可读或可写数据时，rg 不为 pdReady 时那么rg此时就是*g ,将rg原
	// 子设置为 pdReady，然后返回 *g，），某个 g 消费掉这些数据后会把
	// rg 赋值为nil.
	//
	// pdWait 【状态转移：0(nil) -> *g（由 netpollblock 负责状态变更）】
	// 表示相关协程还未被挂起，挂起之后会将其变更为 *g （ gopark( netpollblockcommit )
	// 的 netpollblockcommit 回调中设置）。
	//
	rg atomic.Uintptr // pdReady, pdWait, G waiting for read or pdNil
	wg atomic.Uintptr // pdReady, pdWait, G waiting for write or pdNil

	// 用来保护pollDesc结构中字段。（线程锁）
	lock    mutex // protects the following fields
	// 表示文件描述符正在从poller中移除。
	closing bool
	// 在Linux 下没有用到，aix、Solaris等会利用它来存储一些扩展信息。
	user    uint32    // user settable cookie
	// 一个自增序列号，因为 pollDesc 结构会被复用，通过增加 rseq 的值，能够避免复用的 pollDesc 被旧的
	// 读超时timer干扰。
	rseq    uintptr   // protects from stale read timers
	// 用于实现读超时的 timer ，它会在超时时间到达时唤醒等待的 goroutine。
	rt      timer     // read deadline timer (set if rt.f != nil)
	// 设置超时到期的时间戳(纳秒)。
	rd      int64     // read deadline (a nanotime in the future, -1 when expired)
	wseq    uintptr   // protects from stale write timers
	wt      timer     // write deadline timer
	wd      int64     // write deadline (a nanotime in the future, -1 when expired)
	self    *pollDesc // storage for indirect interface. See (*pollDesc).makeArg.
}

// pollInfo is the bits needed by netpollcheckerr, stored atomically,
// mostly duplicating state that is manipulated under lock in pollDesc.
// The one exception is the pollEventErr bit, which is maintained only
// in the pollInfo.
type pollInfo uint32

const (
	pollClosing = 1 << iota
	// 由 pollDesc.setEventErr() 函数根据 ev.flags(kqueue)或ev.Events(epoll)设置
	pollEventErr
	// 由 pollDesc.publishInfo() 函数设置。
	pollExpiredReadDeadline
	pollExpiredWriteDeadline
)

func (i pollInfo) closing() bool              { return i&pollClosing != 0 }
func (i pollInfo) eventErr() bool             { return i&pollEventErr != 0 }
func (i pollInfo) expiredReadDeadline() bool  { return i&pollExpiredReadDeadline != 0 }
func (i pollInfo) expiredWriteDeadline() bool { return i&pollExpiredWriteDeadline != 0 }

// info returns the pollInfo corresponding to pd.
func (pd *pollDesc) info() pollInfo {
	return pollInfo(pd.atomicInfo.Load())
}

// publishInfo updates pd.atomicInfo (returned by pd.info)
// using the other values in pd.
// It must be called while holding pd.lock,
// and it must be called after changing anything
// that might affect the info bits.
// In practice this means after changing closing
// or changing rd or wd from < 0 to >= 0.
//
// 当 pd.closing、 pd.rd 或 pd.wd 变更时
// 应调用 publishInfo() 以便反应到 pd.atomicInfo的值的对应位上。
//
func (pd *pollDesc) publishInfo() {
	var info uint32
	if pd.closing {
		info |= pollClosing
	}
	if pd.rd < 0 {
		info |= pollExpiredReadDeadline
	}
	if pd.wd < 0 {
		info |= pollExpiredWriteDeadline
	}

	// Set all of x except the pollEventErr bit.
	//
	// 即保留 pollEventErr 位，其它位根据上面计算出来
	// 的 info 设置。
	//
	x := pd.atomicInfo.Load()
	for !pd.atomicInfo.CompareAndSwap(x, (x&pollEventErr)|info) {
		x = pd.atomicInfo.Load()
	}
}

// setEventErr()
// 在 netpoll 函数中调用 (netpoll_epoll.go/netpoll_kqueue.go)
// 根据 ev.flags==_EV_ERROR (kqueue), ev.Events==syscall.EPOLLERR(epoll)
// setEventErr sets the result of pd.info().eventErr() to b.
func (pd *pollDesc) setEventErr(b bool) {
	x := pd.atomicInfo.Load()
	for (x&pollEventErr != 0) != b && !pd.atomicInfo.CompareAndSwap(x, x^pollEventErr) {
		x = pd.atomicInfo.Load()
	}
}

type pollCache struct {
	lock  mutex
	first *pollDesc
	// PollDesc objects must be type-stable,
	// because we can get ready notification from epoll/kqueue
	// after the descriptor is closed/reused.
	// Stale notifications are detected using seq variable,
	// seq is incremented when deadlines are changed or descriptor is reused.
}

var (
	netpollInitLock mutex
	netpollInited   atomic.Uint32

	pollcache      pollCache
	// 统计有多少个协程正在等待netpoller中关联的fd上的到来事件。
	// findrunnable 会根据此值是否大于0来决定 netpoll 的策略。
	netpollWaiters atomic.Uint32
)

//go:linkname poll_runtime_pollServerInit internal/poll.runtime_pollServerInit
func poll_runtime_pollServerInit() {
	netpollGenericInit()
}

// 初始化 poller ，只会被调用一次。在 Linux系统上主要用来创建 epoll 实例，还会创建
// 一个非阻塞式的 pipe, 用来唤醒阻塞中的 netpoller。
// efpd 、 netpollBreakRd 和 netpollBreakWr 都是包级别的变量。
// efpd 是epoll实例的文件描述符。
// netpollBreakRd 和 netpollBreakWr 是非阻塞管道两端的文件描述发，分别被用作读端和写端。
// 读取端 netpollBreakRd 被添加到 epoll 中的监听 EPOLLIN 事件，后续从写入端 netpollBreakWr 写入数据
// 就能唤醒阻塞中的 poller。
//
func netpollGenericInit() {
	if netpollInited.Load() == 0 {
		lockInit(&netpollInitLock, lockRankNetpollInit)
		lock(&netpollInitLock)
		if netpollInited.Load() == 0 {
			netpollinit()
			netpollInited.Store(1)
		}
		unlock(&netpollInitLock)
	}
}

// netpoller 已经被初始化过了。
func netpollinited() bool {
	return netpollInited.Load() != 0
}

//go:linkname poll_runtime_isPollServerDescriptor internal/poll.runtime_isPollServerDescriptor

// poll_runtime_isPollServerDescriptor reports whether fd is a
// descriptor being used by netpoll.
//
// 同时监听文件描述发fd的读写事件
// 用来判断文件描述符fd是否被poller使用，在Linux对应的实现中，
// 只有 epfd 、 netpollBreakRd 和 netpollBreakWr 属于poller使用的描述符。
//
func poll_runtime_isPollServerDescriptor(fd uintptr) bool {
	return netpollIsPollDescriptor(fd)
}

// 用来把要监听的文件描述符fd和与之关联的 pollDesc 结构添加到poller实例中，在Linux 上就是添加到epoll中。
// Linux系统，文件描述符以EPOLLET（监听边缘触发模式）被添加到epoll 中的，同时监听读、写事件。
// pollDesc 类型的结构 pd 作为与fd关联的自定义数据被一同添加到epoll中。
// evt.Data = &pollDesc 。
//
//go:linkname poll_runtime_pollOpen internal/poll.runtime_pollOpen
func poll_runtime_pollOpen(fd uintptr) (*pollDesc, int) {
	pd := pollcache.alloc()
	lock(&pd.lock)
	wg := pd.wg.Load()
	if wg != pdNil && wg != pdReady {
		throw("runtime: blocked write on free polldesc")
	}
	rg := pd.rg.Load()
	if rg != pdNil && rg != pdReady {
		throw("runtime: blocked read on free polldesc")
	}
	// 初始化 fd
	pd.fd = fd
	pd.closing = false
	// 初始时间应该没有错误
	pd.setEventErr(false)
	// 递增读写序列号，复用pd需要更新以区分旧的pd
	pd.rseq++
	pd.rg.Store(pdNil)
	// 清空deadline
	pd.rd = 0
	// 递增写序列号，复用pd需要更新以区别于旧的pd。
	pd.wseq++
	pd.wg.Store(pdNil)
	// 清空写 deadline
	pd.wd = 0
	pd.self = pd
	// 下面会清空pd.atomicInfo中的错误位
	// 因为上面已经将pd.atomicInfo设置来源的各个
	// 字段已经清空了。
	pd.publishInfo()
	unlock(&pd.lock)

	errno := netpollopen(fd, pd)
	if errno != 0 {
		pollcache.free(pd)
		return nil, int(errno)
	}
	return pd, 0
}

//go:linkname poll_runtime_pollClose internal/poll.runtime_pollClose
func poll_runtime_pollClose(pd *pollDesc) {
	// 已经在 evict() 中调用 poll_runtime_pollUnblock 时设置过了。
	if !pd.closing {
		throw("runtime: close polldesc w/o unblock")
	}
	wg := pd.wg.Load()
	// netpollunblock 中已经确定了
	if wg != pdNil && wg != pdReady {
		throw("runtime: blocked write on closing polldesc")
	}
	rg := pd.rg.Load()
 	// netpollunblock 中已经确定了
	if rg != pdNil && rg != pdReady {
		throw("runtime: blocked read on closing polldesc")
	}
	// 关闭底层文件表示符与之关联的事件自动清除。
	netpollclose(pd.fd)
	//  释放 pollDesc 结构到缓存。
	pollcache.free(pd)
}

func (c *pollCache) free(pd *pollDesc) {
	lock(&c.lock)
	pd.link = c.first
	c.first = pd
	unlock(&c.lock)
}

// poll_runtime_pollReset, which is internal/poll.runtime_pollReset,
// prepares a descriptor for polling in mode, which is 'r' or 'w'.
// This returns an error code; the codes are defined above.
//
//go:linkname poll_runtime_pollReset internal/poll.runtime_pollReset
func poll_runtime_pollReset(pd *pollDesc, mode int) int {
	errcode := netpollcheckerr(pd, int32(mode))
	if errcode != pollNoError {
		return errcode
	}
	if mode == 'r' {
		pd.rg.Store(pdNil)
	} else if mode == 'w' {
		pd.wg.Store(pdNil)
	}
	return pollNoError
}

// poll_runtime_pollWait, which is internal/poll.runtime_pollWait,
// waits for a descriptor to be ready for reading or writing,
// according to mode, which is 'r' or 'w'.
// This returns an error code; the codes are defined above.
//
//go:linkname poll_runtime_pollWait internal/poll.runtime_pollWait
func poll_runtime_pollWait(pd *pollDesc, mode int) int {
	errcode := netpollcheckerr(pd, int32(mode))
	if errcode != pollNoError {
		return errcode
	}
	// As for now only Solaris, illumos, and AIX use level-triggered IO.
	if GOOS == "solaris" || GOOS == "illumos" || GOOS == "aix" {
		// netpollarm 函数只有在应用水平触发的系统上才会被用到。
		netpollarm(pd, mode)
	}
	for !netpollblock(pd, int32(mode), false) {
		errcode = netpollcheckerr(pd, int32(mode))
		if errcode != pollNoError {
			return errcode
		}
		// Can happen if timeout has fired and unblocked us,
		// but before we had a chance to run, timeout has been reset.
		// Pretend it has not happened and retry.
	}
	return pollNoError
}

//go:linkname poll_runtime_pollWaitCanceled internal/poll.runtime_pollWaitCanceled
func poll_runtime_pollWaitCanceled(pd *pollDesc, mode int) {
	// This function is used only on windows after a failed attempt to cancel
	// a pending async IO operation. Wait for ioready, ignore closing or timeouts.
	for !netpollblock(pd, int32(mode), true) {
	}
}
// poll_runtime_pollSetDeadline()
//
// 如果 d 为0：
//		1.mode对应的超时已经设置会将timer删除,不会设新的，pd.rd或pd.wd加1。
//      2.mode对应的超时未设置没有效果。
// 如果 d 小于0：
//      1.mode对应的事件未设置timer，直接尝试唤醒监听pd+mode事件的gs。
//      2.mode对应的事件设置了timer，递增对应的seq删除timer，然后尝试唤醒监听pd+mode事件的gs。
// 如果 d 大于0：
//      1.mode对应的事件未设置timer，为pd+mode事件设置timer。
//      2.mode对应的时机设置了tiemr，递增对应的seq，并重置对应的timer。
//
// pd.*seq的作用可以保证在seq递增后，所有之前关联pd.*t即使得到执行其f也是无操作。
// 因为接下来的modifying timer需要时间。
//
// 参数：
// pd：文件描述符绑定的 pollDesc 结构。
// d： deadline截止时间。
// mode: 'r'、'w'或 'r+w'，在什么事件上设置deadline。
//
//go:linkname poll_runtime_pollSetDeadline internal/poll.runtime_pollSetDeadline
func poll_runtime_pollSetDeadline(pd *pollDesc, d int64, mode int) {
	lock(&pd.lock)
	// 已经关闭了设置deadline是没有意义的。
	if pd.closing {
		unlock(&pd.lock)
		return
	}
	// rd0、wd0存储旧的deadline
	rd0, wd0 := pd.rd, pd.wd
	combo0 := rd0 > 0 && rd0 == wd0
	if d > 0 {
		d += nanotime()
		if d <= 0 {
			// If the user has a deadline in the future, but the delay calculation
			// overflows, then set the deadline to the maximum possible value.
			d = 1<<63 - 1
		}
	}
	// 溢出处理end

	// 设置pd.rd和pd.wd
	if mode == 'r' || mode == 'r'+'w' {
		pd.rd = d
	}
	if mode == 'w' || mode == 'r'+'w' {
		pd.wd = d
	}

	// 因为 pd.rd和pd.wd发生了变更，所以要更新 pd.atomicInfo。
	// 可能存在pd.rd或pd.wd设置小于0的情况。
	pd.publishInfo()

	// 如果为true，表示读写超时都设置相等的且未来的时间。
	combo := pd.rd > 0 && pd.rd == pd.wd
	rtf := netpollReadDeadline
	if combo {
		rtf = netpollDeadline
	}
	if pd.rt.f == nil {
		// 之前未设置读超时
		if pd.rd > 0 {
			pd.rt.f = rtf
			// Copy current seq into the timer arg.
			// Timer func will check the seq against current descriptor seq,
			// if they differ the descriptor was reused or timers were reset.
			pd.rt.arg = pd.makeArg()
			pd.rt.seq = pd.rseq
			resettimer(&pd.rt, pd.rd)
		}
	} else if pd.rd != rd0 || combo != combo0 {
		// 之前也设置了读超时但时间不同，或者时间虽相同但设置是通用处理函数或者特别处理
		// 函数与现在的不同。
		pd.rseq++ // invalidate current timers
		if pd.rd > 0 {
			modtimer(&pd.rt, pd.rd, 0, rtf, pd.makeArg(), pd.rseq)
		} else {
			// 如果以前设置了且现在设的超时已经过期
			// 需删除就得计时器。
			deltimer(&pd.rt)
			pd.rt.f = nil
		}
	}
	if pd.wt.f == nil {
		if pd.wd > 0 && !combo {
			// 如果读超时与写超时不同分别设置
			pd.wt.f = netpollWriteDeadline
			pd.wt.arg = pd.makeArg()
			pd.wt.seq = pd.wseq
			resettimer(&pd.wt, pd.wd)
		}
	} else if pd.wd != wd0 || combo != combo0 {
		pd.wseq++ // invalidate current timers
		if pd.wd > 0 && !combo {
			// 如果读超时与写超时不同分别设置
			modtimer(&pd.wt, pd.wd, 0, netpollWriteDeadline, pd.makeArg(), pd.wseq)
		} else {
			// 现在的超时已经到期且存在就得超时计时器
			// 删除旧的。
			deltimer(&pd.wt)
			pd.wt.f = nil
		}
	}
	// If we set the new deadline in the past, unblock currently pending IO if any.
	// Note that pd.publishInfo has already been called, above, immediately after modifying rd and wd.
	var rg, wg *g
	if pd.rd < 0 {
		// 处理当前设置的超时已经到期的逻辑
		rg = netpollunblock(pd, 'r', false)
	}
	if pd.wd < 0 {
		// 处理当前设置的超时已经到期的逻辑
		wg = netpollunblock(pd, 'w', false)
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 3)
	}
	if wg != nil {
		netpollgoready(wg, 3)
	}
}
// poll_runtime_pollUnblock()，目前只被: poll.pollDesc.evict 函数
// 所调用。
//
// 1.pd.closing 设置为true，递增rseq和wseq字段。
// 2.将关联的超时 timer 删除（如果存在的话）
// 3.调用 pollDesc.publishInfo 更新 atomicInfo 字段的相应的标志位。
// 4.将fd关联的阻塞g(读和写，如果存在的话)唤醒
//
//go:linkname poll_runtime_pollUnblock internal/poll.runtime_pollUnblock
func poll_runtime_pollUnblock(pd *pollDesc) {
	lock(&pd.lock)
	if pd.closing {
		throw("runtime: unblock on closing polldesc")
	}
	pd.closing = true
	pd.rseq++
	pd.wseq++
	var rg, wg *g
	// 因为 pd.closing 发生了变更，
	// 更新 pd.atomicInfo的值。
	pd.publishInfo()
	rg = netpollunblock(pd, 'r', false)
	wg = netpollunblock(pd, 'w', false)
	if pd.rt.f != nil {
		deltimer(&pd.rt)
		pd.rt.f = nil
	}
	if pd.wt.f != nil {
		deltimer(&pd.wt)
		pd.wt.f = nil
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 3)
	}
	if wg != nil {
		netpollgoready(wg, 3)
	}
}

// netpollready is called by the platform-specific netpoll function.
// It declares that the fd associated with pd is ready for I/O.
// The toRun argument is used to build a list of goroutines to return
// from netpoll. The mode argument is 'r', 'w', or 'r'+'w' to indicate
// whether the fd is ready for reading or writing or both.
//
// This may run while the world is stopped, so write barriers are not allowed.
//
// 根据mode的值判定是 pd 上关联的fd的可读还是可写事件就绪了
// 然后调用 netpollunblock ,从pd.wg 或者 pd.rg 字段获取关联的 g，
// 如果g不为nil则添加到 gList 链表中。
//
// 例如 mode 的值是可读或可读可写，而 pollDesc 中也有等待读事件的
// goroutine,那么这个 goroutine
// 就应该被唤醒继续运行了，所以就会把这个goroutine添加到toRun中。
// 从pollDesc中获得对应G指针的操作是由 netpollunblock 函数完成的。
//
//go:nowritebarrier
func netpollready(toRun *gList, pd *pollDesc, mode int32) {
	var rg, wg *g
	if mode == 'r' || mode == 'r'+'w' {
		// 有可读事件，尝试唤醒监听此读事件的g如果有的话。
		rg = netpollunblock(pd, 'r', true)
	}
	if mode == 'w' || mode == 'r'+'w' {
		// 有可写事件，尝试唤醒监听此写事件的g如果有的话。
		wg = netpollunblock(pd, 'w', true)
	}
	if rg != nil {
		toRun.push(rg)
	}
	if wg != nil {
		toRun.push(wg)
	}
}
// netpollcheckerr()
// 可能包含以下错误：
// 1. 关联文件描述符已经关闭 pollErrClosing
// 2. 读或写超时 pollErrTimeout
// 3. pollErrNotPollable
func netpollcheckerr(pd *pollDesc, mode int32) int {
	info := pd.info()
	if info.closing() {
		return pollErrClosing
	}
	if (mode == 'r' && info.expiredReadDeadline()) || (mode == 'w' && info.expiredWriteDeadline()) {
		return pollErrTimeout
	}
	// Report an event scanning error only on a read event.
	// An error on a write event will be captured in a subsequent
	// write call that is able to report a more specific error.
	if mode == 'r' && info.eventErr() {
		return pollErrNotPollable
	}
	return pollNoError
}

func netpollblockcommit(gp *g, gpp unsafe.Pointer) bool {
	//
	// 如果 pdWait -> *g 成功 则返回true，否则返回false。
	// 返回false会立即恢复gp的执行。
	//
	// 出现返回false情况时可能是：关闭fd，或者 deadline 到期，
	// 它们的处理逻辑会将 pdWait -> 0，同时在 pd.atomicInfo 上
	// 设置相应的错误位。
	// 停靠失败的g恢复运行后会检查是否设置了错误位，如果设置了
	// 会直接返回错误码到上层逻辑，并不会重试停靠。
	r := atomic.Casuintptr((*uintptr)(gpp), pdWait, uintptr(unsafe.Pointer(gp)))
	if r {
		// Bump the count of goroutines waiting for the poller.
		// The scheduler uses this to decide whether to block
		// waiting for the poller if there is nothing else to do.
		netpollWaiters.Add(1)
	}
	return r
}

func netpollgoready(gp *g, traceskip int) {
	netpollWaiters.Add(-1)
	goready(gp, traceskip+1)
}

// returns true if IO is ready, or false if timed out or closed
// waitio - wait only for completed IO, ignore errors
// Concurrent calls to netpollblock in the same mode are forbidden, as pollDesc
// can hold only a single waiting goroutine for each mode.
//
// 目的：
// 将当前g停靠，等待 pd所关联的描述符上mode事件就绪。
// 正常情况由 netpoll 唤醒，或者有 closing和deadline机制
// 唤醒。
//
// 参数：waitio 表示是否阻塞等待，即使有错误也停靠（目前只发现在Windows上有用true值）
//
//	mode 'r'或者'w'不会同时是'r+w'，因为一个协程只关联一个事件。
//	pd 与事件所有模式符绑定定，是协程与netpoll机制之间的桥梁。
//
// 返回值 如果为true表示io就绪，
// false则可能是：
//
//	1.超时(deadline机制)
//	2.FD.Close时使用ioready=false调用了runtime_pollUnblock。
func netpollblock(pd *pollDesc, mode int32, waitio bool) bool {
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	// set the gpp semaphore to pdWait
	// 下面的for循环会将 pd.wg 或pd.rg 设置为0或者pdWait
	// 为0表表示事件已经就绪可以直接返回。
	for {
		// Consume notification if already ready.
		// gpp 为 pdReady 表示IO已处于就绪状态，所以直接返回true。
		// 不必停靠。
		if gpp.CompareAndSwap(pdReady, pdNil) {
			return true
		}
		// 如果为0，就先通过CAS把它置为 pdWait，表示当前协程即将挂起等待IO就绪，
		// 然后当前协程会调用 gopark 函数来挂起自己，
		// netpollblockcommit 函数会把当前g的地址赋值给 *gpp。等到挂起的协程会
		// netpoller唤醒后，就从gopark 返回，从 gpp
		// 中获取新的 IO 的状态，继续执行后面的逻辑。
		if gpp.CompareAndSwap(pdNil, pdWait) {
			break
		}

		// Double check that this isn't corrupt; otherwise we'd loop
		// forever.
		//
		// 旧值可能是 pdWait，因为pdWait 只可能由上面的
		// if 语句设置，如果上面设置成功便不会到这，如果没
		// 设置成功到这，v不会是pdWait。
		// 如果未 pdWait 说明使用同一个 pollDesc 也在执行
		// 此函数。
		// 因为 FD 进行读写是经过分别序列化的，通过 FD.fdmux锁
		// 所以正常逻辑下面的if不会为true。
		if v := gpp.Load(); v != pdReady && v != pdNil {
			throw("runtime: double wait")
		}
	}

	// need to recheck error states after setting gpp to pdWait
	// this is necessary because runtime_pollUnblock/runtime_pollSetDeadline/deadlineimpl
	// do the opposite: store to closing/rd/wd, publishInfo, load of rg/wg
	// 停靠之前再次检查，防止中途设置了 closing、 deadline 错误。
	//
	if waitio || netpollcheckerr(pd, mode) == pollNoError {
		// 尝试一次 pdWait->*g，如果不成功继续执行下面的语句。
		gopark(netpollblockcommit, unsafe.Pointer(gpp), waitReasonIOWait, traceEvGoBlockNet, 5)
	}
	// be careful to not lose concurrent pdReady notification
	// 停靠的g恢复运行，或者停靠g失败了：
	old := gpp.Swap(pdNil)
	// old应是 pdReady或者0，不能是其它值。
	//
	// old为0：表示关闭了文件描述符时用ioready=false
	// 调用了runtime_pollUnblock或者关联的deadlineLine
	// 到期了它们会将 pd.wg或者pd.rg设置为0，并在pd.atomicInfo上
	// 设置相应的错误位。
	if old > pdWait {
		throw("runtime: corrupted polldesc")
	}
	// 如果 old 为0，返回false，
	// 上层调用者会验证 pd.atomicInfo 中的错误标志位有没有设置
	// 如果没设置会重新调用此函数。
	return old == pdReady
}

// 目的：
// 1.与mode对应的事件就绪需将 pd.rg或pd.wg设为 pdReady，如果旧值是*g则返回它。
// 2.以ioready=false,调用，想唤醒与pd+mode关联的g，让它们结束等待。一般有两种情况：
//   2.1.上面的逻辑FD调用了Close要关闭文件描述符了。
//	 2.2设置了deadline机制，且到期了。
//
// 因为Go在给文件描述符设置监听事件时无论是否监听读写都会设置这两个事件，所以
// pd+r/w 并不一定关联两个g（读、写），但至少关联一个g。

// 参: mode 只能是 'r' 或者是 'w' 。不会是 'r'+'w'，一个g只监听一个事件。
//
//	pd 与mode所属文件模式符绑定。
//	ioready 事件是否就绪。
//
// 目的是根据 mode 从 pollDesc 的 rg 或 wg 中获取对应的 goroutine
// (如果有的话，即目前存在监听事件的g）并返回。
//
// 结果：
// 同时会更新 rg 或 wg 的状态到 pdReady 或在 0（表示nil)。
//
// 函数返回后：mode是'w'，pd.wg的结果值是 pdReady 或者 0。
//
//	mode是'r'，pd.rg的结果值是 pdReady 或者 0。
//
// 返回的 *g 可能是一个 goroutine的地址或者nil。
func netpollunblock(pd *pollDesc, mode int32, ioready bool) *g {
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	for {
		old := gpp.Load()
		// 对应的事件已经就绪:
		// 1.表示事件所属协程(已经放入队列,或接下来注定放入队列)还没有得到执行或者还未执行到将 pdReady->0的语句。
		// 2.根本没有协程在关注此事件。
		// 以上两种情况都直接返回因为事件已经就绪不必再就绪。
		if old == pdReady {
			return nil
		}
		// 此刻没有关注此事件的g，又因为ioready为false。
		// 从0->0是没意义的，直接返回。
		if old == pdNil && !ioready {
			// Only set pdReady for ioready. runtime_pollWait
			// will check for timeout/cancel before waiting.
			return nil
		}
		var new uintptr
		// 如果 ioready=true，则带表关注事件肯定就绪。
		// 但数据不一定没有被读取，因为可能上次唤醒的g
		// 正在处理上次事件的数据同时也会顺带处理这次到达的数据。
		// 这种情况old肯定不会是*g，所以没关系。
		if ioready {
			new = pdReady
		}
		//
		// 因为同时可能存在其它线程也在操作此pd.wg或pd.rg，
		// old的值可能会发生变更。
		// 所以在发生变更的情况下需再次重复此过程。
		if gpp.CompareAndSwap(old, new) {
			// 1.如果ioready=true调用的话, 至此old肯定是pdWait、*g或者0(new是ioready)：
			//    1. pdWait:说明一个协程正在准备监听此事件，但还未执行到挂起本身的语句。
			//    2. *g 表示一个协程已经处于停靠等待的状态。
			//    3. 0，表示此刻没有协程关注此事件。
			// 2.如果ioready=false调用的话，至此old肯定是pdWait或者*g(new是0)
			//    1. pdWait:说明一个协程正在准备监听此事件，但还未执行到挂起本身的语句。
			//    2. *g 表示一个协程已经处于停靠等待的状态。
			if old == pdWait {
				old = pdNil
			}
			return (*g)(unsafe.Pointer(old))
		}
	}
}

// netpolldeadlineimpl()
// 读写超时执行的函数，即 timer.f
// 主要目的：
// 1.
// 2.
// 参数：
// pd:与文件描述符关联的 pollDesc。
// seq:对文件描述符设置读写超时所构造的timer的seq字段值。
//     如果读写的超时时间相同此值等于当时的 pollDesc.rseq ，否则
//     读超时对应 pollDesc.rseq ，写超时对应 pollDesc.wseq 。
// read:是否用于处理读超时。
// write:是否用于处理写超时。
//
// 文件描述符读写超时的处理函数
// 如果读写超时时间不同:
// timer.f = netpollReadDeadline 或 netpollWriteDeadline，而他们都间接调用了此函数。
// 如果读写超时时间相同:
// read=write=true，读写超时共用同一个timer，
// timer.f = netpollDeadline。
//
func netpolldeadlineimpl(pd *pollDesc, seq uintptr, read, write bool) {
	lock(&pd.lock)
	// Seq arg is seq when the timer was set.
	// If it's stale, ignore the timer event.
	//
	// pd.rseq 保存的是pd的当前resq。
	// 因为存在pd复用，所以rseq可能不是当初设置超时的rseq了。
	currentSeq := pd.rseq
	if !read {
		// 如果 read为false，currentSeq就设置为写seq。
		currentSeq = pd.wseq
	}
	// 如果 read和write都为true则它们共用一个timer。
	// 取pd.rseq即可，因为当初如果combo为true时设置timer时取的也是pd.rseq。

	if seq != currentSeq {
		// pd已被重用，超时处理函数不做任何动作。
		// The descriptor was reused or timers were reset.
		//
		// pd被重用(pd回收后并不会删除与之关联的定时器,但重用时pd.rseq和pd.wseq会递增)，
		// 或者 timer被重置（设置了新的超时但与旧值不同）
		//
		// The descriptor was reused or timers were reset.
		unlock(&pd.lock)
		return
	}

	var rg *g
	if read {
		//
		// 在超时处理函数执行到这里不可能遇到 pd.rd或pd.wd小于等于0的情况。
		// 因为到这里说明pd未被重用，超时函数未被执行。
		// 1. 设置pd.rd或pd.wd小于0即-1由此函数在下面设置。
		// 2. 设置超时时如果截止时间为负值修正为1<<63-1，为0会直接唤醒阻塞中的g、删除timer(如果它们存在的话)。
		if pd.rd <= 0 || pd.rt.f == nil {
			throw("runtime: inconsistent read deadline")
		}
		// 此函数得到调用说明已经超时。
		pd.rd = -1
		// 更新 pd.atomicInfo 上的读超时标志位。
		pd.publishInfo()
		// 尝试唤醒等待 pd+r事件的g。
		rg = netpollunblock(pd, 'r', false)
	}
	var wg *g
	if write {
		if pd.wd <= 0 || pd.wt.f == nil && !read {
			throw("runtime: inconsistent write deadline")
		}
		pd.wd = -1
		pd.publishInfo()
		wg = netpollunblock(pd, 'w', false)
	}
	unlock(&pd.lock)
	if rg != nil {
		// 加入任务队列等待执行
		netpollgoready(rg, 0)
	}
	if wg != nil {
		// 加入任务队列等待执行
		netpollgoready(wg, 0)
	}
}
// 当读写超时时间相同时：pd.rt.f
func netpollDeadline(arg any, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, true)
}
// 当读写超时时间不同时：pd.rt.f
func netpollReadDeadline(arg any, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, false)
}
// 当读写超时时间相同时：pd.wt.f
func netpollWriteDeadline(arg any, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, false, true)
}

func (c *pollCache) alloc() *pollDesc {
	lock(&c.lock)
	if c.first == nil {
		const pdSize = unsafe.Sizeof(pollDesc{})
		n := pollBlockSize / pdSize
		if n == 0 {
			n = 1
		}
		// Must be in non-GC memory because can be referenced
		// only from epoll/kqueue internals.
		mem := persistentalloc(n*pdSize, 0, &memstats.other_sys)
		for i := uintptr(0); i < n; i++ {
			pd := (*pollDesc)(add(mem, i*pdSize))
			pd.link = c.first
			c.first = pd
		}
	}
	pd := c.first
	c.first = pd.link
	lockInit(&pd.lock, lockRankPollDesc)
	unlock(&c.lock)
	return pd
}

// makeArg converts pd to an interface{}.
// makeArg does not do any allocation. Normally, such
// a conversion requires an allocation because pointers to
// types which embed runtime/internal/sys.NotInHeap (which pollDesc is)
// must be stored in interfaces indirectly. See issue 42076.
func (pd *pollDesc) makeArg() (i any) {
	x := (*eface)(unsafe.Pointer(&i))
	x._type = pdType
	x.data = unsafe.Pointer(&pd.self)
	return
}

var (
	pdEface any    = (*pollDesc)(nil)
	pdType  *_type = efaceOf(&pdEface)._type
)
