// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package runtime

// Integrated network poller (kqueue-based implementation).

import (
	"runtime/internal/atomic"
	"unsafe"
)

var (
	kq int32 = -1

	netpollBreakRd, netpollBreakWr uintptr // for netpollBreak

	netpollWakeSig atomic.Uint32 // used to avoid duplicate calls of netpollBreak
)

func netpollinit() {
	kq = kqueue()
	if kq < 0 {
		println("runtime: kqueue failed with", -kq)
		throw("runtime: netpollinit failed")
	}
	// 当fork子进程后，仍然可以使用fd。但执行exec后系统就会字段关闭子进程中的fd了。
	closeonexec(kq)
	// 创建一个读写非阻塞管道用于唤醒阻塞中的netpoll。
	r, w, errno := nonblockingPipe()
	if errno != 0 {
		println("runtime: pipe failed with", -errno)
		throw("runtime: pipe failed")
	}
	ev := keventt{
		filter: _EVFILT_READ,
		flags:  _EV_ADD,
	}
	*(*uintptr)(unsafe.Pointer(&ev.ident)) = uintptr(r)
	// 将读端添加到kq事件列表。
	n := kevent(kq, &ev, 1, nil, 0, nil)
	if n < 0 {
		println("runtime: kevent failed with", -n)
		throw("runtime: kevent failed")
	}
	// 读端描述符
	netpollBreakRd = uintptr(r)
	// 写端描述符
	netpollBreakWr = uintptr(w)
}

func netpollIsPollDescriptor(fd uintptr) bool {
	return fd == uintptr(kq) || fd == netpollBreakRd || fd == netpollBreakWr
}

// 用来把fd和与之关联的 pollDesc 结构添加到 poller 实例中。
func netpollopen(fd uintptr, pd *pollDesc) int32 {
	// Arm both EVFILT_READ and EVFILT_WRITE in edge-triggered mode (EV_CLEAR)
	// for the whole fd lifetime. The notifications are automatically unregistered
	// when fd is closed.
	var ev [2]keventt
	*(*uintptr)(unsafe.Pointer(&ev[0].ident)) = fd
	ev[0].filter = _EVFILT_READ
	ev[0].flags = _EV_ADD | _EV_CLEAR
	ev[0].fflags = 0
	ev[0].data = 0
	// 用户数据,保留用户程度定义的数据信息
	ev[0].udata = (*byte)(unsafe.Pointer(pd))
	ev[1] = ev[0] // 两个 keventt 结构
	ev[1].filter = _EVFILT_WRITE
	// &ev[0] 第一个 keventt 结构的指针。
	// 同时监听fd上的读写。
	n := kevent(kq, &ev[0], 2, nil, 0, nil)
	if n < 0 {
		return -n
	}
	return 0
}

func netpollclose(fd uintptr) int32 {
	// Don't need to unregister because calling close()
	// on fd will remove any kevents that reference the descriptor.
	return 0
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// netpollBreak interrupts a kevent.
// 用来唤醒阻塞中的 netpoll ，它实际上就是向 netpollBreakWr 描述符中写入数据，这样一来 epoll 就会监听到 netpollBreakRd 的 EPOLLIN 事件。
func netpollBreak() {
	// Failing to cas indicates there is an in-flight wakeup, so we're done here.
	if !netpollWakeSig.CompareAndSwap(0, 1) {
		return
	}

	for {
		var b byte
		n := write(netpollBreakWr, unsafe.Pointer(&b), 1)
		if n == 1 || n == -_EAGAIN {
			break
		}
		// 因为write 调用可能会被打断，所以在遇到 EINTR 错误的时候，
		// netpollBreak 函数会通过for循环持续尝试向 netpollBreakWr
		// 中写入一个字节数据。
		if n == -_EINTR {
			continue
		}
		println("runtime: netpollBreak write failed with", -n)
		throw("runtime: netpollBreak write failed")
	}
}

// netpoll checks for ready network connections.
// Returns list of goroutines that become runnable.
// delay < 0: blocks indefinitely
// delay == 0: does not block, just polls
// delay > 0: block for up to that many nanoseconds
//
// 参数 delay，指定阻塞时间，为0表示不阻塞。
// 否则为poll阻塞的时间,不同平台有不同的最大值要求，超过的值会被纠正。
// gList 已经准备就绪的文件描述符列表所关联的 goroutine构成的列表。
// 根据epoll 返回的IO事件标志位为mode赋值：r表示可读，w表示可写，r+w表示即可读又可写。
// mode不为0，表示有IO事件，需要从ev.data字段得到与IO事件关联的 pollDesc ，
// 检测IO事件中的错误标志位，并相应地为 pd.ererr 赋值
// 最后调用 netpollready 函数。将关联的 g添加到 gLIst 链表。
//
// 返回值：因为IO数据就绪而能够恢复运行的一组g。
//
func netpoll(delay int64) gList {
	// kq未初始化之前的值就是-1。
	// poller 未初始化，返回空列表。
	// 因为文件模式符不可能小于0。
	if kq == -1 {
		return gList{}
	}
	var tp *timespec
	var ts timespec
	if delay < 0 {
		tp = nil
	} else if delay == 0 {
		tp = &ts
	} else {
		ts.setNsec(delay)
		if ts.tv_sec > 1e6 {
			// Darwin returns EINVAL if the sleep time is too long.
			// 修正最大值
			// Darwin returns EINVAL if the sleep time is too long.
			ts.tv_sec = 1e6
		}
		tp = &ts
	}
	//保存已经有数据的事件，也就是一次最多能返回64个就绪事件。
	var events [64]keventt
retry:
	n := kevent(kq, nil, 0, &events[0], int32(len(events)), tp)
	if n < 0 {//遇到错误
		if n != -_EINTR {
			println("runtime: kevent on fd", kq, "failed with", -n)
			throw("runtime: netpoll failed")
		}
		// If a timed sleep was interrupted, just return to
		// recalculate how long we should sleep now.
		// 如果设置了超时并遇到错误，
		// 返回空列表调用者可以选择重新计算超时时间。
		if delay > 0 {
			return gList{}
		}
		// 其它错误重试
		goto retry
	}
	var toRun gList
	for i := 0; i < int(n); i++ {
		ev := &events[i]

		if uintptr(ev.ident) == netpollBreakRd {
			// 跳过netpollBreakRd
			if ev.filter != _EVFILT_READ {
				println("runtime: netpoll: break fd ready for", ev.filter)
				throw("runtime: netpoll: break fd ready for something unexpected")
			}
			if delay != 0 {
				// netpollBreak could be picked up by a
				// nonblocking poll. Only read the byte
				// if blocking.
				var tmp [16]byte
				// 读取缓存里的数据以便下次可以阻塞，不会因为 netpollBreakRd
				// 已经可读。
				read(int32(netpollBreakRd), noescape(unsafe.Pointer(&tmp[0])), int32(len(tmp)))
				netpollWakeSig.Store(0)
			}
			continue
		}

		var mode int32
		// 如果有事件发生会在 ev.filter 中设置相应
		// 的标志位。
		switch ev.filter {
		case _EVFILT_READ:
			mode += 'r'

			// On some systems when the read end of a pipe
			// is closed the write end will not get a
			// _EVFILT_WRITE event, but will get a
			// _EVFILT_READ event with EV_EOF set.
			// Note that setting 'w' here just means that we
			// will wake up a goroutine waiting to write;
			// that goroutine will try the write again,
			// and the appropriate thing will happen based
			// on what that write returns (success, EPIPE, EAGAIN).
			if ev.flags&_EV_EOF != 0 {
				mode += 'w'
			}
		case _EVFILT_WRITE:
			mode += 'w'
		}
		if mode != 0 {
			pd := (*pollDesc)(unsafe.Pointer(ev.udata))
			pd.setEventErr(ev.flags == _EV_ERROR)
			netpollready(&toRun, pd, mode)
		}
	}
	return toRun
}
