// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"runtime/internal/atomic"
	"unsafe"
)

// defined constants
const (
	// G status
	//
	// Beyond indicating the general state of a G, the G status
	// acts like a lock on the goroutine's stack (and hence its
	// ability to execute user code).
	//
	// If you add to this list, add to the list
	// of "okay during garbage collection" status
	// in mgcmark.go too.
	//
	// TODO(austin): The _Gscan bit could be much lighter-weight.
	// For example, we could choose not to run _Gscanrunnable
	// goroutines found in the run queue, rather than CAS-looping
	// until they become _Grunnable. And transitions like
	// _Gscanwaiting -> _Gscanrunnable are actually okay because
	// they don't affect stack ownership.

	// _Gidle means this goroutine was just allocated and has not
	// yet been initialized.
	// goroutine 刚刚被分配，还没有被初始化
	_Gidle = iota // 0

	// _Grunnable means this goroutine is on a run queue. It is
	// not currently executing user code. The stack is not owned.
	// goroutine 在某个 runq 中，当期那没有在运行用户代码，它的栈不归自己所有
	_Grunnable // 1

	// _Grunning means this goroutine may execute user code. The
	// stack is owned by this goroutine. It is not on a run queue.
	// It is assigned an M and a P (g.m and g.m.p are valid).
	// goroutine 可能正在运行用户代码，它的栈归自己所有。运行中的goroutine不在
	// 一个 runq 中，并且有关联的M
	_Grunning // 2

	// _Gsyscall means this goroutine is executing a system call.
	// It is not executing user code. The stack is owned by this
	// goroutine. It is not on a run queue. It is assigned an M.
	// goroutine 正在执行一个系统调用，而且没有在执行用户代码。它的栈归自己
	// 所有，不在任何一个 runq 中，并且有关联的 M
	_Gsyscall // 3

	// _Gwaiting means this goroutine is blocked in the runtime.
	// It is not executing user code. It is not on a run queue,
	// but should be recorded somewhere (e.g., a channel wait
	// queue) so it can be ready()d when necessary. The stack is
	// not owned *except* that a channel operation may read or
	// write parts of the stack under the appropriate channel
	// lock. Otherwise, it is not safe to access the stack after a
	// goroutine enters _Gwaiting (e.g., it may get moved).
	// goroutine 阻塞在 runtime 中，没有在执行用户代码。它不在任何 runq 中
	// 但是应该被记录在其他地方，例如一个 channel 的等待队列中。它的栈不归自己
	// 所有，除非 channel 操作将会在 channel lock的保护下读写栈上的部分数据
	// 。否则，当一个goroutine进入 _Gwaiting 后再去访问它的栈是不安全的。
	_Gwaiting // 4

	// _Gmoribund_unused is currently unused, but hardcoded in gdb
	// scripts.
	_Gmoribund_unused // 5

	// _Gdead means this goroutine is currently unused. It may be
	// just exited, on a free list, or just being initialized. It
	// is not executing user code. It may or may not have a stack
	// allocated. The G and its stack (if any) are owned by the M
	// that is exiting the G or that obtained the G from the free
	// list.
	// goroutine 当前没有被用到，它可能刚刚退出执行，在一个空闲链表中，或者
	// 刚刚完成初始化。他没有在执行用户代码，可能分配了栈，也可能没有。G和它
	// 的栈（如果有）由退出的G或从空闲链表中获得G的M所有
	// 使用 runtime.newproc 创建G实例时，会将G的状态设置为 _Gdead，方式
	// GC 扫描它的未初始化栈
	_Gdead // 6

	// _Genqueue_unused is currently unused.
	_Genqueue_unused // 7

	// _Gcopystack means this goroutine's stack is being moved. It
	// is not executing user code and is not on a run queue. The
	// stack is owned by the goroutine that put it in _Gcopystack.
	// goroutine 的栈正在被移动。它没有在执行用户代码，也不在 runq 中。栈的
	// 所有权归把当前goroutine设置为 _Gcopystack 状态的 groutine 所有。
	_Gcopystack // 8

	// _Gpreempted means this goroutine stopped itself for a
	// suspendG preemption. It is like _Gwaiting, but nothing is
	// yet responsible for ready()ing it. Some suspendG must CAS
	// the status to _Gwaiting to take responsibility for
	// ready()ing this G.
	// goroutine 因为 suspendG 抢占而停止。该状态和 _Gwaiting 很像，但没有谁
	// 负责将 groutine 设置为就绪状态。 一些 suspendG 必须原子性地把该状态
	// 转换为 _Gwaiting 状态，并负责重新将该 groutine 置为就绪状态。调用 resumeG
	_Gpreempted // 9

	// _Gscan combined with one of the above states other than
	// _Grunning indicates that GC is scanning the stack. The
	// goroutine is not executing user code and the stack is owned
	// by the goroutine that set the _Gscan bit.
	//
	// _Gscanrunning is different: it is used to briefly block
	// state transitions while GC signals the G to scan its own
	// stack. This is otherwise like _Grunning.
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to when the scan completes.
	// 标志位 _Gscan 与上述的一些状态组合，可以得到 _Gscanrunnable / _Gscanrunning
	// _Gscansyscall _Gscanwaiting 和 _Gscanpreempted 这些组合状态。除 _Gscanrunning
	// 外，其他组合状态都表示GC正在扫描goroutine的栈，goroutine没有执行用户代码，栈的
	// 所有权归设置了 _Gscan 标志位的 goroutine 所有。 _Gscanrunning 有些特性，在GC通知
	// G 扫描栈的时候，它被用来短暂地阻止状态变换，其它方面和 _Grunning 一样。栈扫描完成后，
	// goroutine 将会切换回原来的状态，移除 _Gscan标志。
	_Gscan          = 0x1000
	_Gscanrunnable  = _Gscan + _Grunnable  // 0x1001
	_Gscanrunning   = _Gscan + _Grunning   // 0x1002
	_Gscansyscall   = _Gscan + _Gsyscall   // 0x1003
	_Gscanwaiting   = _Gscan + _Gwaiting   // 0x1004
	_Gscanpreempted = _Gscan + _Gpreempted // 0x1009
)

const (
	// P status

	// _Pidle means a P is not being used to run user code or the
	// scheduler. Typically, it's on the idle P list and available
	// to the scheduler, but it may just be transitioning between
	// other states.
	//
	// The P is owned by the idle list or by whatever is
	// transitioning its state. Its run queue is empty.
	// 空闲状态，此时的P没有被用来执行用户代码或调度代码，通常位于空闲链表中
	// 能够被调度器获取，它的状态可能正在由空闲转变为其他状态。P 的所有权归
	// 空闲链表或某个正在改变它状态的线程所有，本地 runq 为空。
	_Pidle = iota

	// _Prunning means a P is owned by an M and is being used to
	// run user code or the scheduler. Only the M that owns this P
	// is allowed to change the P's status from _Prunning. The M
	// may transition the P to _Pidle (if it has no more work to
	// do), _Psyscall (when entering a syscall), or _Pgcstop (to
	// halt for the GC). The M may also hand ownership of the P
	// off directly to another M (e.g., to schedule a locked G).
	// 运行中状态。当期P正被某个M所持有，并且用于执行用户代码或调度器代码。只有
	// 持有P所有权的M，才允许将P的状态从 _Prunning 转变为其它状态。在任务都执行
	// 完毕以后，M会把P设置为_Pidle状态。在进入系统调用时，M会把P设置为_Psyscall
	// 状态。挂起以执行GC时，会设置为 _Pgcstop 状态。某些情况下，M还可能直接把P
	// 的所有权交给另一个M
	_Prunning

	// _Psyscall means a P is not running user code. It has
	// affinity to an M in a syscall but is not owned by it and
	// may be stolen by another M. This is similar to _Pidle but
	// uses lightweight transitions and maintains M affinity.
	//
	// Leaving _Psyscall must be done with a CAS, either to steal
	// or retake the P. Note that there's an ABA hazard: even if
	// an M successfully CASes its original P back to _Prunning
	// after a syscall, it must understand the P may have been
	// used by another M in the interim.
	// 系统调用状态。此时P没有执行用户代码，它和一个处于syscall中的M间存在弱
	// 关联关系，可能会被另一个M窃取走
	_Psyscall

	// _Pgcstop means a P is halted for STW and owned by the M
	// that stopped the world. The M that stopped the world
	// continues to use its P, even in _Pgcstop. Transitioning
	// from _Prunning to _Pgcstop causes an M to release its P and
	// park.
	//
	// The P retains its run queue and startTheWorld will restart
	// the scheduler on Ps with non-empty run queues.
	// GC 停止状态。P被 STW 挂起以执行GC，所有权给执行 STW 的M所有，执行 STW
	// 的 M 会继续使用处于 _Pgcstop 状态的P，当P的状态从 _Prunning 转变为
	// _Pgcstop 时，会造成关联的M释放P的所有权，然后进入阻塞状态。 P会保留它的本地
	// runq ,然后 Start The World 会重新启动这些本地 runq不为空的P。
	_Pgcstop

	// _Pdead means a P is no longer used (GOMAXPROCS shrank). We
	// reuse Ps if GOMAXPROCS increases. A dead P is mostly
	// stripped of its resources, though a few things remain
	// (e.g., trace buffers).
	// 停用状态。因为 GOMAXPROCS 收缩，会造成多余的P被停用，当 GOMAXPROCS 再次
	// 增大时还会被复用。高一二停用的P，大部分资源被剥夺，只有很少量保留
	_Pdead
)

// Mutual exclusion locks.  In the uncontended case,
// as fast as spin locks (just a few user-level instructions),
// but on the contention path they sleep in the kernel.
// A zeroed Mutex is unlocked (no need to initialize each lock).
// Initialization is helpful for static lock ranking, but not required.
type mutex struct {
	// Empty struct if lock ranking is disabled, otherwise includes the lock rank
	lockRankStruct
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

// sleep and wakeup on one-time events.
// before any calls to notesleep or notewakeup,
// must call noteclear to initialize the Note.
// then, exactly one thread can call notesleep
// and exactly one thread can call notewakeup (once).
// once notewakeup has been called, the notesleep
// will return.  future notesleep will return immediately.
// subsequent noteclear must be called only after
// previous notesleep has returned, e.g. it's disallowed
// to call noteclear straight after notewakeup.
//
// notetsleep is like notesleep but wakes up after
// a given number of nanoseconds even if the event
// has not yet happened.  if a goroutine uses notetsleep to
// wake up early, it must wait to call noteclear until it
// can be sure that no other goroutine is calling
// notewakeup.
//
// notesleep/notetsleep are generally called on g0,
// notetsleepg is similar to notetsleep but is called on user g.
type note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

type funcval struct {
	fn uintptr
	// variable-size, fn-specific data here
}
// iface 对应非空接口内部数据结构
type iface struct {
	tab  *itab
	data unsafe.Pointer
}

type eface struct {
	_type *_type
	data  unsafe.Pointer
}

func efaceOf(ep *any) *eface {
	return (*eface)(unsafe.Pointer(ep))
}

// The guintptr, muintptr, and puintptr are all used to bypass write barriers.
// It is particularly important to avoid write barriers when the current P has
// been released, because the GC thinks the world is stopped, and an
// unexpected write barrier would not be synchronized with the GC,
// which can lead to a half-executed write barrier that has marked the object
// but not queued it. If the GC skips the object and completes before the
// queuing can occur, it will incorrectly free the object.
//
// We tried using special assignment functions invoked only when not
// holding a running P, but then some updates to a particular memory
// word went through write barriers and some did not. This breaks the
// write barrier shadow checking mode, and it is also scary: better to have
// a word that is completely ignored by the GC than to have one for which
// only a few updates are ignored.
//
// Gs and Ps are always reachable via true pointers in the
// allgs and allp lists or (during allocation before they reach those lists)
// from stack variables.
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an muintptr across a safe point.

// A guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//
// The Gobuf.g goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.g's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using guintptr doesn't make that problem any worse.
// Note that pollDesc.rg, pollDesc.wg also store g in uintptr form,
// so they would need to be updated too if g's start moving.
type guintptr uintptr

//go:nosplit
func (gp guintptr) ptr() *g { return (*g)(unsafe.Pointer(gp)) }

//go:nosplit
func (gp *guintptr) set(g *g) { *gp = guintptr(unsafe.Pointer(g)) }

//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
}

// setGNoWB performs *gp = new without a write barrier.
// For times when it's impractical to use a guintptr.
//
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) {
	(*guintptr)(unsafe.Pointer(gp)).set(new)
}

type puintptr uintptr

//go:nosplit
func (pp puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

//go:nosplit
func (pp *puintptr) set(p *p) { *pp = puintptr(unsafe.Pointer(p)) }

// muintptr is a *m that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
//  1. Never hold an muintptr locally across a safe point.
//
//  2. Any muintptr in the heap must be owned by the M itself so it can
//     ensure it is not in use when the last true *m is released.
type muintptr uintptr

//go:nosplit
func (mp muintptr) ptr() *m { return (*m)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *muintptr) set(m *m) { *mp = muintptr(unsafe.Pointer(m)) }

// setMNoWB performs *mp = new without a write barrier.
// For times when it's impractical to use an muintptr.
//
//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) {
	(*muintptr)(unsafe.Pointer(mp)).set(new)
}

type gobuf struct {
	// The offsets of sp, pc, and g are known to (hard-coded in) libmach.
	//
	// ctxt is unusual with respect to GC: it may be a
	// heap-allocated funcval, so GC needs to track it, but it
	// needs to be set and cleared from assembly, where it's
	// difficult to have write barriers. However, ctxt is really a
	// saved, live register, and we only ever exchange it between
	// the real register and the gobuf. Hence, we treat it as a
	// root during stack scanning, which means assembly that saves
	// and restores it doesn't need write barriers. It's still
	// typed as a pointer so that any other writes from Go get
	// write barriers.
	sp   uintptr  // 栈指针
	pc   uintptr  // 指令指针
	g    guintptr // 用来反向关联到对应的G
	ctxt unsafe.Pointer //指向闭包对象，也就是说明用go关键字创建协程的时候传递的是一个闭包
	// 这里会存储闭包对象的地址。
	ret  uintptr // 用来存储返回值，实际上利用AX寄存器来实现类似C函数的返回值，且目前只发现 panic-recover
	// 机制中用到了该字段。
	lr   uintptr // lr 在arm等架构上用来存储返回地址，x86没有用到该字段。
	bp   uintptr // for framepointer-enabled architectures
	// 用来存储栈帧基址
}

// sudog represents a g in a wait list, such as for sending/receiving
// on a channel.
//
// sudog is necessary because the g ↔ synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.
type sudog struct {
	// The following fields are protected by the hchan.lock of the
	// channel this sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.

	g *g

	next *sudog
	prev *sudog
	elem unsafe.Pointer // data element (may point to stack)

	// The following fields are never accessed concurrently.
	// For channels, waitlink is only accessed by g.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot lock.

	acquiretime int64
	releasetime int64
	ticket      uint32

	// isSelect indicates g is participating in a select, so
	// g.selectDone must be CAS'd to win the wake-up race.
	isSelect bool

	// success indicates whether communication over channel c
	// succeeded. It is true if the goroutine was awoken because a
	// value was delivered over channel c, and false if awoken
	// because c was closed.
	success bool

	parent   *sudog // semaRoot binary tree
	waitlink *sudog // g.waiting list or semaRoot
	waittail *sudog // semaRoot
	c        *hchan // channel
}

type libcall struct {
	fn   uintptr
	n    uintptr // number of parameters
	args uintptr // parameters
	r1   uintptr // return values
	r2   uintptr
	err  uintptr // error number
}

// Stack describes a Go execution stack.
// The bounds of the stack are exactly [lo, hi),
// with no implicit data structures on either side.
type stack struct {
	lo uintptr
	hi uintptr
}

// heldLockInfo gives info on a held lock and the rank of that lock
type heldLockInfo struct {
	lockAddr uintptr
	rank     lockRank
}

type g struct {
	// Stack parameters.
	// stack describes the actual stack memory: [stack.lo, stack.hi).
	// stackguard0 is the stack pointer compared in the Go stack growth prologue.
	// It is stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
	// stackguard1 is the stack pointer compared in the C stack growth prologue.
	// It is stack.lo+StackGuard on g0 and gsignal stacks.
	// It is ~0 on other goroutine stacks, to trigger a call to morestackc (and crash).
	stack       stack   // offset known to runtime/cgo
	stackguard0 uintptr // offset known to liblink
	stackguard1 uintptr // offset known to liblink

	_panic    *_panic // innermost panic - offset known to liblink
	_defer    *_defer // innermost defer
	m         *m      // current m; offset known to arm liblink
	sched     gobuf   // 调度器用来保存 goroutine 的执行上下文
	syscallsp uintptr // if status==Gsyscall, syscallsp = sched.sp to use during gc
	syscallpc uintptr // if status==Gsyscall, syscallpc = sched.pc to use during gc
	// 在mac os平台为: g.stack.hi - 32字节
	// 在 runtime.newproc1 中设置的
	stktopsp  uintptr // expected sp at top of stack, to check in traceback
	// param is a generic pointer parameter field used to pass
	// values in particular contexts where other storage for the
	// parameter would be difficult to find. It is currently used
	// in three ways:
	// 1. When a channel operation wakes up a blocked goroutine, it sets param to
	//    point to the sudog of the completed blocking operation.
	// 2. By gcAssistAlloc1 to signal back to its caller that the goroutine completed
	//    the GC cycle. It is unsafe to do so in any other way, because the goroutine's
	//    stack may have moved in the meantime.
	// 3. By debugCallWrap to pass parameters to a new goroutine because allocating a
	//    closure in the runtime is forbidden.
	param        unsafe.Pointer
	atomicstatus atomic.Uint32
	stackLock    uint32 // sigprof/scang lock; TODO: fold in to atomicstatus
	goid         uint64
	schedlink    guintptr  // 被调度器用来实现内部链表、队列，对应的 guintptr 类型从
	// 逻辑上讲等价于 *g，而底层类型却是个 uintptr，这样为了避免写屏障。
	waitsince    int64      // approx time when the g become blocked
	waitreason   waitReason // if status==Gwaiting

	preempt       bool // preemption signal, duplicates stackguard0 = stackpreempt
	// 当为true时，调度器会在合适的时机触发一次抢占
	preemptStop   bool // transition to _Gpreempted on preemption; otherwise, just deschedule
	preemptShrink bool // shrink stack at synchronous safe point

	// asyncSafePoint is set if g is stopped at an asynchronous
	// safe point. This means there are frames on the stack
	// without precise pointer information.
	asyncSafePoint bool

	paniconfault bool // panic (instead of crash) on unexpected fault address
	gcscandone   bool // g has scanned stack; protected by _Gscan bit in status
	throwsplit   bool // must not split stack
	// activeStackChans indicates that there are unlocked channels
	// pointing into this goroutine's stack. If true, stack
	// copying needs to acquire channel locks to protect these
	// areas of the stack.
	activeStackChans bool
	// parkingOnChan indicates that the goroutine is about to
	// park on a chansend or chanrecv. Used to signal an unsafe point
	// for stack shrinking.
	parkingOnChan atomic.Bool

	raceignore     int8     // ignore race detection events
	sysblocktraced bool     // StartTrace has emitted EvGoInSyscall about this goroutine
	tracking       bool     // whether we're tracking this G for sched latency statistics
	trackingSeq    uint8    // used to decide whether to track this G
	trackingStamp  int64    // timestamp of when the G last started being tracked
	runnableTime   int64    // the amount of time spent runnable, cleared when running, only used when tracking
	sysexitticks   int64    // cputicks when syscall has returned (for tracing)
	traceseq       uint64   // trace event sequencer
	tracelastp     puintptr // last P emitted an event for this goroutine
	lockedm        muintptr // 关联到与当前G绑定的M，可以参考 LockOSthread
	sig            uint32
	writebuf       []byte
	sigcode0       uintptr
	sigcode1       uintptr
	sigpc          uintptr
	// runtime.newproc 的 return address
	gopc           uintptr         // pc of go statement that created this goroutine
	// 存储着g的祖先goroutines
	ancestors      *[]ancestorInfo // ancestor information goroutine(s) that created this goroutine (only used if debug.tracebackancestors)
	// goroutine 开始执行的第一条指令，即goroutine关联函数的第一条指令
	startpc        uintptr         // pc of goroutine function
	racectx        uintptr
	waiting        *sudog         // sudog structures this g is waiting on (that have a valid elem ptr); in lock order
	// 主要用于实现 channel 中的等待队列
	cgoCtxt        []uintptr      // cgo traceback context
	// only user goroutines have labels
	// labels can be inheired
	labels         unsafe.Pointer // profiler labels
	timer          *timer         // cached timer for time.Sleep
	// runtime 内部实现的计时器类型，主要用来支持 time.Sleep
	selectDone     atomic.Uint32  // are we participating in a select and did someone win the race?

	// goroutineProfiled indicates the status of this goroutine's stack for the
	// current in-progress goroutine profile
	goroutineProfiled goroutineProfileStateHolder

	// Per-G GC state

	// gcAssistBytes is this G's GC assist credit in terms of
	// bytes allocated. If this is positive, then the G has credit
	// to allocate gcAssistBytes bytes without assisting. If this
	// is negative, then the G must correct this by performing
	// scan work. We track this in bytes to make it fast to update
	// and check for debt in the malloc hot path. The assist ratio
	// determines how this corresponds to scan work debt.
	gcAssistBytes int64
}

// gTrackingPeriod is the number of transitions out of _Grunning between
// latency tracking runs.
const gTrackingPeriod = 8

const (
	// tlsSlots is the number of pointer-sized slots reserved for TLS on some platforms,
	// like Windows.
	tlsSlots = 6
	tlsSize  = tlsSlots * goarch.PtrSize
)

// Values for m.freeWait.
const (
	freeMStack = 0 // M done, free stack and reference.
	freeMRef   = 1 // M done, free reference.
	freeMWait  = 2 // M still in use.
)

type m struct {
	g0      *g     // goroutine with scheduling stack
	// 并不是一个真正的 gorotuine，它的栈是由操作系统分配的，初始化大小比普通goroutine栈要大
	// 被用作调度器执行的栈
	morebuf gobuf  // gobuf arg to morestack
	divmod  uint32 // div/mod denominator for arm - known to liblink
	_       uint32 // align next field to 8 bytes

	// Fields not known to debuggers.
	// pthread_self() 函数调用的结果应该是线程ID
	procid        uint64            // for debuggers, but offset not hard-coded
	gsignal       *g                // signal-handling g
	// 本质上是用来处理信号的栈，因为一些UNIX系统支持为信号处理器配置独立的栈
	goSigStack    gsignalStack      // Go-allocated signal handling stack
	sigmask       sigset            // storage for saved signal mask
	tls           [tlsSlots]uintptr // thread-local storage (for x86 extern register)
	mstartfn      func()
	curg          *g       // current running goroutine
	// 指向M正在执行的G
	caughtsig     guintptr // goroutine running during fatal signal
	p             puintptr // attached p for executing go code (nil if not executing go code)
	// GMP 中的P，即关联到当前M上的处理器
	nextp         puintptr // 用来将P传递给M，调度器一般是在M阻塞时为m.nextp赋值，等到
	// M 开始运行后会尝试从nextp处获取P进行关联
	oldp          puintptr // the p that was attached before executing a syscall
	// 用来暂存执行系统调用之前关联的P
	id            int64    // M的唯一ID
	mallocing     int32
	throwing      throwType
	preemptoff    string // if != "", keep curg running on this m
	// 不为空时表示关闭对curg的抢占，字符串内容给出了相关的原因。
	locks         int32 // 记录当前M持有锁的数量，不为0时能够阻止抢占发生
	dying         int32
	profilehz     int32
	spinning      bool // m is out of work and is actively looking for work
	// 表示当前M正处于自旋状态
	blocked       bool // m is blocked on a note
	// 如果将 alternate signal stack 设置为了 m.gsignal.stck ，此字段为true
	// 如果将 m.goSigStack 设置位 alternate signal stack，此字段为false
	newSigstack   bool // minit on C thread called sigaltstack
	printlock     int8
	incgo         bool          // m is executing a cgo call
	isextra       bool          // m is an extra m
	freeWait      atomic.Uint32 // Whether it is safe to free g0 and delete m (one of freeMRef, freeMStack, freeMWait)
	fastrand      uint64
	needextram    bool
	traceback     uint8
	ncgocall      uint64        // number of cgo calls in total
	ncgo          int32         // number of cgo calls currently in progress
	cgoCallersUse atomic.Uint32 // if non-zero, cgoCallers in use temporarily
	//if iscgo || GOOS == "solaris" || GOOS == "illumos" || GOOS == "windows" {
	//mp.cgoCallers = new(cgoCallers)}
	cgoCallers    *cgoCallers   // cgo traceback if crashing in cgo call
	park          note  // 用来支持M的挂起与唤醒，可以很方便地实施每个M单独
	// 挂起与唤醒
	alllink       *m // on allm  把所有的M串联起来，构成 allm 链表
	schedlink     muintptr 	// 被调度器用于实现链表，构成空闲链表
	lockedg       guintptr  // 关联到与当前M绑定的G
	createstack   [32]uintptr // stack that created this thread.
	lockedExt     uint32      // tracking for external LockOSThread
	lockedInt     uint32      // tracking for internal lockOSThread
	nextwaitm     muintptr    // next m waiting for lock
	waitunlockf   func(*g, unsafe.Pointer) bool
	waitlock      unsafe.Pointer
	waittraceev   byte
	waittraceskip int
	startingtrace bool
	syscalltick   uint32  // 用来把已经退出运行的M连起来，构成 sched.freem 链表，
	// 方便下次分配时使用
	freelink      *m // on sched.freem

	// these are here because they are too large to be on the stack
	// of low-level NOSPLIT functions.
	libcall   libcall
	libcallpc uintptr // for cpu profiler
	libcallsp uintptr
	libcallg  guintptr
	syscall   libcall // stores syscall parameters on windows

	vdsoSP uintptr // SP for traceback while in VDSO call (0 if not in call)
	vdsoPC uintptr // PC for traceback while in VDSO call

	// preemptGen counts the number of completed preemption
	// signals. This is used to detect when a preemption is
	// requested, but fails.
	preemptGen atomic.Uint32

	// Whether this is a pending preemption signal on this M.
	signalPending atomic.Uint32

	dlogPerM

	mOS

	// Up to 10 locks held by this m, maintained by the lock ranking code.
	locksHeldLen int
	locksHeld    [10]heldLockInfo
}

type p struct {
	id          int32 // P的唯一ID相当于当前P在allp数组中的下标
	status      uint32 // one of pidle/prunning/...
	// 表示P的状态
	link        puintptr // 是一个没有写屏障的指针，被调度器用来构成链表
	schedtick   uint32     // incremented on every scheduler call
	// 记录了调度发生的次数，实际上在每一次 goroutine 切换且不继承时间片的
	// 情况下，该字段会加一
	syscalltick uint32     // incremented on every system call
	// 每发生一次系统调用就会加一
	sysmontick  sysmontick // last tick observed by sysmon
	// 被监控线程用来存储上一次检查时的调度器时钟滴答，用以实现时间片
	m           muintptr   // back-link to associated m (nil if idle)
	// 本质上是一个指针，反向关联到P绑定的M
	mcache      *mcache
	pcache      pageCache
	raceprocctx uintptr

	deferpool    []*_defer // pool of available defer structs (see panic.go)
	deferpoolbuf [32]*_defer

	// Cache of goroutine ids, amortizes accesses to runtime·sched.goidgen.
	goidcache    uint64
	goidcacheend uint64 // 用来宠全局 sched.goidgen 处申请goid分配区间，批量申请以减少
	// 全局范围的锁争用，所以G的编号并不是顺序生成。

	// status
	// Queue of runnable goroutines. Accessed without lock.
	runqhead uint32
	runqtail uint32 // 当前P的就绪队列，用一个数组和一头一尾两个下标实现了一个环形
	// 队列
	runq     [256]guintptr
	// runnext, if non-nil, is a runnable G that was ready'd by
	// the current G and should be run next instead of what's in
	// runq if there's time remaining in the running G's time
	// slice. It will inherit the time left in the current time
	// slice. If a set of goroutines is locked in a
	// communicate-and-wait pattern, this schedules that set as a
	// unit and eliminates the (potentially large) scheduling
	// latency that otherwise arises from adding the ready'd
	// goroutines to the end of the run queue.
	//
	// Note that while other P's may atomically CAS this to zero,
	// only the owner P can CAS it to a valid G.
	// 如果不为nil，则指向一个被当前G准备好（就绪）的G，接下来将继承当前G的时间
	// 片开始运行。该字段存在的意义在于，假如有一组goroutine中有生产者和消费
	// 者，它们在一个channel上频繁地等待、唤醒，那么调度器会把它们作为一个单元
	// 来调度，每次使用 runnext 比添加到本地 runq 尾部能大幅减少延迟
	runnext guintptr

	// Available G's (status == Gdead)
	// 用来缓存已经退出的G，方便再次分配时进行使用
	gFree struct {
		gList
		n int32
	}

	sudogcache []*sudog
	sudogbuf   [128]*sudog

	// Cache of mspan objects from the heap.
	mspancache struct {
		// We need an explicit length here because this field is used
		// in allocation codepaths where write barriers are not allowed,
		// and eliminating the write barrier/keeping it eliminated from
		// slice updates is tricky, moreso than just managing the length
		// ourselves.
		len int
		buf [128]*mspan
	}

	tracebuf traceBufPtr

	// traceSweep indicates the sweep events should be traced.
	// This is used to defer the sweep start event until a span
	// has actually been swept.
	traceSweep bool
	// traceSwept and traceReclaimed track the number of bytes
	// swept and reclaimed by sweeping in the current sweep loop.
	traceSwept, traceReclaimed uintptr

	palloc persistentAlloc // per-P to avoid mutex

	// The when field of the first entry on the timer heap.
	// This is 0 if the timer heap is empty.
	timer0When atomic.Int64

	// The earliest known nextwhen field of a timer with
	// timerModifiedEarlier status. Because the timer may have been
	// modified again, there need not be any timer with this value.
	// This is 0 if there are no timerModifiedEarlier timers.
	timerModifiedEarliest atomic.Int64

	// Per-P GC state
	gcAssistTime         int64 // Nanoseconds in assistAlloc
	gcFractionalMarkTime int64 // Nanoseconds in fractional mark worker (atomic)

	// limiterEvent tracks events for the GC CPU limiter.
	limiterEvent limiterEvent

	// gcMarkWorkerMode is the mode for the next mark worker to run in.
	// That is, this is used to communicate with the worker goroutine
	// selected for immediate execution by
	// gcController.findRunnableGCWorker. When scheduling other goroutines,
	// this field must be set to gcMarkWorkerNotWorker.
	gcMarkWorkerMode gcMarkWorkerMode
	// gcMarkWorkerStartTime is the nanotime() at which the most recent
	// mark worker started.
	gcMarkWorkerStartTime int64

	// gcw is this P's GC work buffer cache. The work buffer is
	// filled by write barriers, drained by mutator assists, and
	// disposed on certain GC state transitions.
	gcw gcWork

	// wbBuf is this P's GC write barrier buffer.
	//
	// TODO: Consider caching this in the running G.
	wbBuf wbBuf

	runSafePointFn uint32 // if 1, run sched.safePointFn at next safe point

	// statsSeq is a counter indicating whether this P is currently
	// writing any stats. Its value is even when not, odd when it is.
	statsSeq atomic.Uint32

	// Lock for timers. We normally access the timers while running
	// on this P, but the scheduler can also do it from a different P.
	timersLock mutex

	// Actions to take at some time. This is used to implement the
	// standard library's time package.
	// Must hold timersLock to access.
	timers []*timer

	// Number of timers in P's heap.
	numTimers atomic.Uint32

	// Number of timerDeleted timers in P's heap.
	deletedTimers atomic.Uint32

	// Race context used while executing timer functions.
	timerRaceCtx uintptr

	// maxStackScanDelta accumulates the amount of stack space held by
	// live goroutines (i.e. those eligible for stack scanning).
	// Flushed to gcController.maxStackScan once maxStackScanSlack
	// or -maxStackScanSlack is reached.
	maxStackScanDelta int64

	// gc-time statistics about current goroutines
	// Note that this differs from maxStackScan in that this
	// accumulates the actual stack observed to be used at GC time (hi - sp),
	// not an instantaneous measure of the total stack size that might need
	// to be scanned (hi - lo).
	scannedStackSize uint64 // stack size of goroutines scanned by this P
	scannedStacks    uint64 // number of goroutines scanned by this P

	// preempt is set to indicate that this P should be enter the
	// scheduler ASAP (regardless of what G is running on it).
	// 在 Go1.14 版本中引入以支持新的异步抢占机制
	preempt bool

	// pageTraceBuf is a buffer for writing out page allocation/free/scavenge traces.
	//
	// Used only if GOEXPERIMENT=pagetrace.
	pageTraceBuf pageTraceBuf

	// Padding is no longer needed. False sharing is now not a worry because p is large enough
	// that its size class is an integer multiple of the cache line size (for any of our architectures).
}

type schedt struct {
	// 用作全局的 goid 分配器，以保 goid 的唯一性。 P中的 goidcache 就是从这里批量获取 goid 的
	goidgen   atomic.Uint64
	// 记录的是上一次执行 netpoll 的时间，如果等于0，则表示某个线程正在阻塞式地执行 netpoll
	lastpoll  atomic.Int64 // time of last network poll, 0 if currently polling
	// 表示阻塞式的 netpoll 将在何时被唤醒。 Go1.14 版本重构了 Timer ，引入该字段，唤醒
	// netpoller 以处理 Timer。在netpoll阻塞调用返回后会将此值设置为0.
	pollUntil atomic.Int64 // time to which current poll is sleeping

	// 全局范围的调度器锁，访问 sched 中很多字段需要提前获取该锁
	lock mutex

	// When increasing nmidle, nmidlelocked, nmsys, or nmfreed, be
	// sure to call checkdead().

	// 空闲M链表的表头， nmidle 记录的是空闲 M 的数量，即链表的长度
	midle        muintptr // idle m's waiting for work
	//
	nmidle       int32    // number of idle m's waiting for work
	// 统计的是与G绑定（LockOSThread）且处于空闲状态的M，绑定的G没有在运行，
	// 相应的M不能用来运行其它G，只能挂起，以便进入空闲状态。
	nmidlelocked int32    // number of locked m's waiting for work
	// 记录了共创建了多少个M，同时也被用作下一个M的ID
	mnext        int64    // number of m's that have been created and next M ID
	// 限制了最多允许的M的个数，除去那些已经释放的
	maxmcount    int32    // maximum number of m's allowed (or die)
	// 统计的是系统M的个数，这些M不在检查死锁的范围内
	nmsys        int32    // number of system m's not counted for deadlock
	// 统计的是累计已经释放了多少M
	nmfreed      int64    // cumulative number of freed m's

	// 记录的是系统goroutine的数量，会被原子性地更新
	// 以 runtime.isSystemGoroutine 为判断依据
	ngsys atomic.Int32 // number of system goroutines

	// 空闲P链表的表头， npidle 记录了空闲P的个数，也就是链表的长度
	pidle        puintptr // idle p's
	npidle       atomic.Int32
	// 记录的是处于自旋状态的M的数量
	nmspinning   atomic.Int32  // See "Worker thread parking/unparking" comment in proc.go.
	needspinning atomic.Uint32 // See "Delicate dance" comment in proc.go. Boolean. Must hold sched.lock to set to 1.

	// Global runnable queue.
	// 全局就绪队列
	runq     gQueue
	// 记录的是全局就绪队列的长度
	runqsize int32

	// disable controls selective disabling of the scheduler.
	//
	// Use schedEnableUser to control this.
	//
	// disable is protected by sched.lock.
	// 用来禁止调度用户 goroutine，其中的user变量被置为true后，调度器不再
	// 调度执行用户goroutine，系统goroutine不受影响。期间就绪的用户goroutine
	// 会被临时存放到 disable.runnable 队列中，变量n记录了队列的长度
	disable struct {
		// user disables scheduling of user goroutines.
		user     bool
		runnable gQueue // pending runnable Gs
		n        int32  // length of runnable
	}

	// Global cache of dead G's.
	// 用来缓存已退出运行的G，lock 是本结构单独的锁，避免争用 sched.lock。 stack
	// 和 noStacK 这两个列表分别用来存储有栈和没有栈的G，因为在G结束运行被回收的时
	// 候，如果栈大小超过了标准大小，就会被释放，所以有一部分G是没有栈的。变量n是两个
	// 列表长度之和，也就是总共缓冲了多少个G。
	gFree struct {
		lock    mutex
		stack   gList // Gs with stacks
		noStack gList // Gs without stacks
		n       int32
	}

	// Central cache of sudog structs.
	// 构成了sudog结构的中央缓存，供各个P存取
	sudoglock  mutex
	sudogcache *sudog

	// Central pool of available defer structs.
	// 构成一个 _defer 结构的中央缓存
	deferlock mutex
	deferpool *_defer

	// freem is the list of m's waiting to be freed when their
	// m.exited is set. Linked through m.freelink.
	// 一组已经结束运行的M构成的链表的表头，通过 m.freelink 链表到下一项，链表中
	// 的内容在分配新的M时会被复用
	freem *m

	// 表示GC正在等待运行，和 stopwait 、 stopnote 一同被用于实现 STW。 stopwait
	// 记录了STW需要停止P的数量，发起STW的线程被把GOMAXPROCS赋值给 stopwait,也就是
	// 需要停止所有的P，在把 gcwaiting 置为1，然后在 stopnote 上睡眠等待被唤醒。其它
	// 正处于运行的M检测到 gcwaiting 后会释放关联的P的所有权，并把P的状态置为 _Pgcstop，
	// 在把 gcwaiting 的值减1，然后M把自己挂起。M在自我挂起之前如果检测到 stopwait=0，
	// 也就是所有P都已经停止了，就会通过 stopnote 唤醒发起STW的线程。
	gcwaiting  atomic.Bool // gc is waiting to run
	stopwait   int32
	stopnote   note
	// 不为0时表示监控线程 sysmon 正在 sysmonnote 上睡眠，其它 M 会在适当的时机讲 sysmonwait
	// 置为0，并通过 sysmonnote 唤醒监控线程
	sysmonwait atomic.Bool
	sysmonnote note

	// safepointFn should be called on each P at the next GC
	// safepoint if p.runSafePointFn is set.
	// 是个 Function Value，safePointWait 和 safePointNote 的作用有些类似于 stopwait 和 stopnote,
	// 被 runtime.forEachP 用来确保每个P都在一个GC安全点执行了safePointFn
	safePointFn   func(*p)
	safePointWait int32
	safePointNote note

	// 用来设置性能分析采样的频率
	profilehz int32 // cpu profiling rate

	// 用于统计改变 GOMAXPROCS 所花费的时间
	procresizetime int64 // nanotime() of last change to gomaxprocs
	totaltime      int64 // ∫gomaxprocs dt up to procresizetime

	// sysmonlock protects sysmon's actions on the runtime.
	//
	// Acquire and hold this mutex to block sysmon from interacting
	// with the rest of the runtime.
	// 监控线程 sysmon 访问 runtime 数据时会加上的锁，其它线程可以通过它和监控
	// 线程进行同步。
	sysmonlock mutex

	// timeToRun is a distribution of scheduling latencies, defined
	// as the sum of time a G spends in the _Grunnable state before
	// it transitions to _Grunning.
	timeToRun timeHistogram

	// idleTime is the total CPU time Ps have "spent" idle.
	//
	// Reset on each GC cycle.
	idleTime atomic.Int64

	// totalMutexWaitTime is the sum of time goroutines have spent in _Gwaiting
	// with a waitreason of the form waitReasonSync{RW,}Mutex{R,}Lock.
	totalMutexWaitTime atomic.Int64
}

// Values for the flags field of a sigTabT.
const (
	_SigNotify   = 1 << iota // let signal.Notify have signal, even if from kernel
	_SigKill                 // if signal.Notify doesn't take it, exit quietly
	_SigThrow                // if signal.Notify doesn't take it, exit loudly
	_SigPanic                // if the signal is from the kernel, panic
	_SigDefault              // if the signal isn't explicitly requested, don't monitor it
	_SigGoExit               // cause all runtime procs to exit (only used on Plan 9).
	_SigSetStack             // Don't explicitly install handler, but add SA_ONSTACK to existing libc handler
	_SigUnblock              // always unblock; see blockableSig
	_SigIgn                  // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker
// See https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
type _func struct {
	entryOff uint32 // start pc, as offset from moduledata.text/pcHeader.textStart
	nameOff  int32  // function name, as index into moduledata.funcnametab.

	args        int32  // in/out args size
	deferreturn uint32 // offset of start of a deferreturn call instruction from entry, if any.

	pcsp      uint32
	pcfile    uint32
	pcln      uint32
	npcdata   uint32
	cuOffset  uint32 // runtime.cutab offset of this function's CU
	startLine int32  // line number of start of function (func keyword/TEXT directive)
	funcID    funcID // set for certain special runtime functions
	flag      funcFlag
	_         [1]byte // pad
	nfuncdata uint8   // must be last, must end on a uint32-aligned boundary

	// The end of the struct is followed immediately by two variable-length
	// arrays that reference the pcdata and funcdata locations for this
	// function.

	// pcdata contains the offset into moduledata.pctab for the start of
	// that index's table. e.g.,
	// &moduledata.pctab[_func.pcdata[_PCDATA_UnsafePoint]] is the start of
	// the unsafe point table.
	//
	// An offset of 0 indicates that there is no table.
	//
	// pcdata [npcdata]uint32

	// funcdata contains the offset past moduledata.gofunc which contains a
	// pointer to that index's funcdata. e.g.,
	// *(moduledata.gofunc +  _func.funcdata[_FUNCDATA_ArgsPointerMaps]) is
	// the argument pointer map.
	//
	// An offset of ^uint32(0) indicates that there is no entry.
	//
	// funcdata [nfuncdata]uint32
}

// Pseudo-Func that is returned for PCs that occur in inlined code.
// A *Func can be either a *_func or a *funcinl, and they are distinguished
// by the first uintptr.
type funcinl struct {
	ones      uint32  // set to ^0 to distinguish from _func
	entry     uintptr // entry of the real (the "outermost") frame
	name      string
	file      string
	line      int32
	startLine int32
}

// layout of Itab known to compilers
// allocated in non-garbage-collected memory
// Needs to be in sync with
// ../cmd/compile/internal/reflectdata/reflect.go:/^func.WriteTabs.
type itab struct {
	// 接口元信息
	inter *interfacetype
	// 动态类型元信息
	_type *_type
	hash  uint32 // copy of _type.hash. Used for type switches.
	_     [4]byte
	// 接口方法到动态类型方法的映射
	fun   [1]uintptr // variable sized. fun[0]==0 means _type does not implement inter.
}

// Lock-free stack node.
// Also known to export_test.go.
type lfnode struct {
	next    uint64
	pushcnt uintptr
}

type forcegcstate struct {
	lock mutex
	g    *g
	idle atomic.Bool
}

// extendRandom extends the random numbers in r[:n] to the whole slice r.
// Treats n<0 as n==0.
func extendRandom(r []byte, n int) {
	if n < 0 {
		n = 0
	}
	for n < len(r) {
		// Extend random bits using hash function & time seed
		w := n
		if w > 16 {
			w = 16
		}
		h := memhash(unsafe.Pointer(&r[n-w]), uintptr(nanotime()), uintptr(w))
		for i := 0; i < goarch.PtrSize && n < len(r); i++ {
			r[n] = byte(h)
			n++
			h >>= 8
		}
	}
}

// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in deferProcStack.
// This struct must match the code in cmd/compile/internal/ssagen/ssa.go:deferstruct
// and cmd/compile/internal/ssagen/ssa.go:(*state).call.
// Some defers will be allocated on the stack and some on the heap.
// All defers are logically part of the stack, so write barriers to
// initialize them are not required. All defers must be manually scanned,
// and for heap defers, marked.
type _defer struct {
	started bool
	heap    bool
	// openDefer indicates that this _defer is for a frame with open-coded
	// defers. We have only one defer record for the entire frame (which may
	// currently have 0, 1, or more defers active).
	openDefer bool
	sp        uintptr // sp at time of defer
	pc        uintptr // pc at time of defer
	fn        func()  // can be nil for open-coded defers
	_panic    *_panic // panic that is running defer
	link      *_defer // next defer on G; can point to either heap or stack!

	// If openDefer is true, the fields below record values about the stack
	// frame and associated function that has the open-coded defer(s). sp
	// above will be the sp for the frame, and pc will be address of the
	// deferreturn call in the function.
	fd   unsafe.Pointer // funcdata for the function associated with the frame
	varp uintptr        // value of varp for the stack frame
	// framepc is the current pc associated with the stack frame. Together,
	// with sp above (which is the sp associated with the stack frame),
	// framepc/sp can be used as pc/sp pair to continue a stack trace via
	// gentraceback().
	framepc uintptr
}

// A _panic holds information about an active panic.
//
// A _panic value must only ever live on the stack.
//
// The argp and link fields are stack pointers, but don't need special
// handling during stack growth: because they are pointer-typed and
// _panic values only live on the stack, regular stack pointer
// adjustment takes care of them.
type _panic struct {
	argp      unsafe.Pointer // pointer to arguments of deferred call run during panic; cannot move - known to liblink
	arg       any            // argument to panic
	link      *_panic        // link to earlier panic
	pc        uintptr        // where to return to in runtime if this panic is bypassed
	sp        unsafe.Pointer // where to return to in runtime if this panic is bypassed
	recovered bool           // whether this panic is over
	aborted   bool           // the panic was aborted
	goexit    bool
}

// ancestorInfo records details of where a goroutine was started.
type ancestorInfo struct {
	pcs  []uintptr // pcs from the stack of this goroutine
	goid uint64    // goroutine id of this goroutine; original goroutine possibly dead
	gopc uintptr   // pc of go statement that created this goroutine
}

const (
	_TraceRuntimeFrames = 1 << iota // include frames for internal runtime functions.
	_TraceTrap                      // the initial PC, SP are from a trap, not a return PC from a call
	_TraceJumpStack                 // if traceback is on a systemstack, resume trace at g that called into it
)

// The maximum number of frames we print for a traceback
const _TracebackMaxFrames = 100

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonSelect                                  // "select"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonGCScavengeWait                          // "GC scavenge wait"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGCIdle                             // "force gc (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonSyncMutexLock                           // "sync.Mutex.Lock"
	waitReasonSyncRWMutexRLock                        // "sync.RWMutex.RLock"
	waitReasonSyncRWMutexLock                         // "sync.RWMutex.Lock"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
	waitReasonGCWorkerActive                          // "GC worker (active)"
	waitReasonPreempted                               // "preempted"
	waitReasonDebugCall                               // "debug call"
	waitReasonGCMarkTermination                       // "GC mark termination"
	waitReasonStoppingTheWorld                        // "stopping the world"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonGCScavengeWait:        "GC scavenge wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGCIdle:           "force gc (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonSyncMutexLock:         "sync.Mutex.Lock",
	waitReasonSyncRWMutexRLock:      "sync.RWMutex.RLock",
	waitReasonSyncRWMutexLock:       "sync.RWMutex.Lock",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
	waitReasonGCWorkerActive:        "GC worker (active)",
	waitReasonPreempted:             "preempted",
	waitReasonDebugCall:             "debug call",
	waitReasonGCMarkTermination:     "GC mark termination",
	waitReasonStoppingTheWorld:      "stopping the world",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

func (w waitReason) isMutexWait() bool {
	return w == waitReasonSyncMutexLock ||
		w == waitReasonSyncRWMutexRLock ||
		w == waitReasonSyncRWMutexLock
}

var (
	// Add to allm so garbage collector doesn't free g->m
	// when it is just in a register or thread-local storage.
	allm       *m
	gomaxprocs int32
	ncpu       int32
	forcegc    forcegcstate
	sched      schedt
	newprocs   int32

	// allpLock protects P-less reads and size changes of allp, idlepMask,
	// and timerpMask, and all writes to allp.
	allpLock mutex
	// len(allp) == gomaxprocs; may change at safe points, otherwise
	// immutable.
	allp []*p
	// Bitmask of Ps in _Pidle list, one bit per P. Reads and writes must
	// be atomic. Length may change at safe points.
	//
	// Each P must update only its own bit. In order to maintain
	// consistency, a P going idle must the idle mask simultaneously with
	// updates to the idle P list under the sched.lock, otherwise a racing
	// pidleget may clear the mask before pidleput sets the mask,
	// corrupting the bitmap.
	//
	// N.B., procresize takes ownership of all Ps in stopTheWorldWithSema.
	idlepMask pMask
	// Bitmask of Ps that may have a timer, one bit per P. Reads and writes
	// must be atomic. Length may change at safe points.
	timerpMask pMask

	// Pool of GC parked background workers. Entries are type
	// *gcBgMarkWorkerNode.
	gcBgMarkWorkerPool lfstack

	// Total number of gcBgMarkWorker goroutines. Protected by worldsema.
	gcBgMarkWorkerCount int32

	// Information about what cpu features are available.
	// Packages outside the runtime should not use these
	// as they are not an external api.
	// Set on startup in asm_{386,amd64}.s
	processorVersionInfo uint32
	isIntel              bool

	goarm uint8 // set by cmd/link on arm systems
)

// Set by the linker so the runtime can determine the buildmode.
var (
	islibrary bool // -buildmode=c-shared
	isarchive bool // -buildmode=c-archive
)

// Must agree with internal/buildcfg.FramePointerEnabled.
const framepointer_enabled = GOARCH == "amd64" || GOARCH == "arm64"
