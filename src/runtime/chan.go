// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go channels.

// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement,
//  in which case the length of c.sendq and c.recvq is limited only by the
//  size of the select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.

import (
	"internal/abi"
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	maxAlign  = 8
	hchanSize = unsafe.Sizeof(hchan{}) + uintptr(-int(unsafe.Sizeof(hchan{}))&(maxAlign-1))
	debugChan = false
)

type hchan struct {
	// channel 分为无缓存和有缓存两种，对于有缓存channel来讲，需要有相应的内存
	// 来存储数据，实际上就是一个数组，需要知道数组的地址、容量、元素的大小，以及
	// 数组的长度，也就是已有元素的格式。
	//
	// 数组的长度，即已有元素的个数
	qcount   uint           // total data in the queue
	// 数组容量，即可容纳元素的个数
	dataqsiz uint           // size of the circular queue
	// 数组地址
	buf      unsafe.Pointer // points to an array of dataqsiz elements
	// 元素大小
	elemsize uint16


	// channel 是能够被关闭的，所以要有一个字段记录是否已经关闭了。
	closed   uint32
	// 因为 runtime 中内存复制、垃圾回收等机制依赖数据的类型信息，所有hchan中
	// 还需要有一个指针，指向元素类型的类型元数据。
	elemtype *_type // element type

	// channel支持交替地读写，有缓冲channel内的缓冲数组会被作为一个
	// 环形缓存区使用，当下标超过数据容量后弧回到第一个位置，所以需要
	// 有两个字段记录当前读和写的下标位置。
	//
	// 下一次写下标位置
	sendx    uint   // send index
	// 下一次读下标位置
	recvx    uint   // receive index

	// 当读和写操作不能立即完成时，需要能够让当前协程在channel上等待
	// 当条件满足时，要能够立即唤醒等待的协程，所以要有两个等待队列
	// 分别针对读和写。
	//
	// 读等待队列
	recvq    waitq  // list of recv waiters
	// 写等待队列
	sendq    waitq  // list of send waiters

	// lock protects all fields in hchan, as well as several
	// fields in sudogs blocked on this channel.
	//
	// Do not change another G's status while holding this lock
	// (in particular, do not ready a G), as this can deadlock
	// with stack shrinking.
	//
	// 线程类型的锁。
	// 协程通信间通信肯定涉及并发访问，所以要有锁来保护整个数据结构。
	lock mutex
}

type waitq struct {
	first *sudog
	last  *sudog
}

//go:linkname reflect_makechan reflect.makechan
func reflect_makechan(t *chantype, size int) *hchan {
	return makechan(t, size)
}

func makechan64(t *chantype, size int64) *hchan {
	if int64(int(size)) != size {
		panic(plainError("makechan: size out of range"))
	}

	return makechan(t, int(size))
}

func makechan(t *chantype, size int) *hchan {
	elem := t.elem

	// compiler checks this but be safe.
	if elem.size >= 1<<16 {
		throw("makechan: invalid channel element type")
	}
	if hchanSize%maxAlign != 0 || elem.align > maxAlign {
		throw("makechan: bad alignment")
	}

	mem, overflow := math.MulUintptr(elem.size, uintptr(size))
	if overflow || mem > maxAlloc-hchanSize || size < 0 {
		panic(plainError("makechan: size out of range"))
	}

	// Hchan does not contain pointers interesting for GC when elements stored in buf do not contain pointers.
	// buf points into the same allocation, elemtype is persistent.
	// SudoG's are referenced from their owning thread so they can't be collected.
	// TODO(dvyukov,rlh): Rethink when collector can move allocated objects.
	var c *hchan
	switch {
	case mem == 0:
		// Queue or element size is zero.
		c = (*hchan)(mallocgc(hchanSize, nil, true))
		// Race detector uses this location for synchronization.
		c.buf = c.raceaddr()
	case elem.ptrdata == 0:
		// Elements do not contain pointers.
		// Allocate hchan and buf in one call.
		c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
		c.buf = add(unsafe.Pointer(c), hchanSize)
	default:
		// Elements contain pointers.
		c = new(hchan)
		c.buf = mallocgc(mem, elem, true)
	}

	c.elemsize = uint16(elem.size)
	c.elemtype = elem
	c.dataqsiz = uint(size)
	lockInit(&c.lock, lockRankHchan)

	if debugChan {
		print("makechan: chan=", c, "; elemsize=", elem.size, "; dataqsiz=", size, "\n")
	}
	return c
}

// chanbuf(c, i) is pointer to the i'th slot in the buffer.
func chanbuf(c *hchan, i uint) unsafe.Pointer {
	return add(c.buf, uintptr(i)*uintptr(c.elemsize))
}

// full reports whether a send on c would block (that is, the channel is full).
// It uses a single word-sized read of mutable state, so although
// the answer is instantaneously true, the correct answer may have changed
// by the time the calling function receives the return value.
func full(c *hchan) bool {
	// c.dataqsiz is immutable (never written after the channel is created)
	// so it is safe to read at any time during channel operation.
	if c.dataqsiz == 0 {
		// Assumes that a pointer read is relaxed-atomic.
		return c.recvq.first == nil
	}
	// Assumes that a uint read is relaxed-atomic.
	return c.qcount == c.dataqsiz
}

// entry point for c <- x from compiled code
//
// channel 的常规send操作会被编译器转换成对 runtime.channel1
// 函数的调用，后者内部只是调用了 runtime.chansend()函数。
//
//go:nosplit
func chansend1(c *hchan, elem unsafe.Pointer) {
	chansend(c, elem, true, getcallerpc())
}

/*
 * generic single channel send/recv
 * If block is not nil,
 * then the protocol will not
 * sleep but return if it could
 * not complete.
 *
 * sleep can wake up with g.param == nil
 * when a channel involved in the sleep has
 * been closed.  it is easiest to loop and re-run
 * the operation; we'll see that it's now closed.
 */
// 参数：
// c hchan 指针，指向要用来 send 数据的 channel。
// ep 是一个指针，指向要被送入通道c的数据，数据类型要和c的元素类型一致。
// block 表示如果send操作不能立即完成，是否想要等待。
// callerpc 用以进行race相关检测，暂时不需关心。
// 返回值：
// true 表示数据send完成，false表示目前不能发送，但因为不想阻塞（block为false)而
// 返回。
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
	if c == nil {
		if !block {
			return false
		}
		gopark(nil, nil, waitReasonChanSendNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	if debugChan {
		print("chansend: chan=", c, "\n")
	}

	if raceenabled {
		racereadpc(c.raceaddr(), callerpc, abi.FuncPCABIInternal(chansend))
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	//
	// After observing that the channel is not closed, we observe that the channel is
	// not ready for sending. Each of these observations is a single word-sized read
	// (first c.closed and second full()).
	// Because a closed channel cannot transition from 'ready for sending' to
	// 'not ready for sending', even if the channel is closed between the two observations,
	// they imply a moment between the two when the channel was both not yet closed
	// and not ready for sending. We behave as if we observed the channel at that moment,
	// and report that the send cannot proceed.
	//
	// It is okay if the reads are reordered here: if we observe that the channel is not
	// ready for sending and then observe that it is not closed, that implies that the
	// channel wasn't closed during the first observation. However, nothing here
	// guarantees forward progress. We rely on the side effects of lock release in
	// chanrecv() and closechan() to update this thread's view of c.closed and full().
	//
	// 如果 block 为false且closed为0，也就是在不想阻塞且通道未关闭
	// 的前提下，如果通道满了（无缓存且 recvq为空，或者有缓存且缓冲
	// 已经用尽)，则返回false。
	//
	// 本步判断是在不加锁的情况下进行的，目的是让非阻塞 send在无法立即完成时
	// 能真正不阻塞（加锁操作可能阻塞)。
	// c.closed 和 full(c) 判断都是 'single word-sized read'，所以可以保证读的数据完整。
	// 1. 下面if与有连个观察：c未关闭，c满了。如果在c未关闭和c满了之间通道关闭了，我们认为这样情况
	// 算作不能向c发送。
	// 2. 两个观察的顺序可以改变，同样即使在full(c)和c.closed 之间通道不再满了，我们也认为通道是
	// 不能发送的。
	//
	// 上面两个插入操作都是及其短暂的。所以可以忽略。即极小可能得伪阳，但不会出现伪阴。所以还要继续接
	// 下来的逻辑。
	//
	// 关闭的c通道肯定不能发送数据。但通过c.closed判断的通道没关闭不一定能发送数据: 1. c.closed的
	// 准确读取是靠 closechan() 函数返回时释放的锁，让读取 c.closed 的线程可见到这个 c.closed的
	// 及时值。2. 即使及时确认了 c.closed 未关闭，但发送操作不是原子的，所以还需在加锁状态下进行。
	//
	// c.full() 也是如此，通过c.full()的值为false，不能认为立即的发送操作不会阻塞，立即的接收操作不
	// 会阻塞(可能性不大)。其它线程对 c.full() 的观察需要 chansend()和chanrecv()的锁释放。靠锁创
	// 建临界区。
	//
	if !block && c.closed == 0 && full(c) {
		return false
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)

	// 如果 closed 不为0，即通道已经关闭，则先解锁，然后
	// panic。因为不允许用已关闭的通道进行send。
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("send on closed channel"))
	}

	// 如果 recvq 不为空，隐含了缓冲区为空，就从中取出第一个排队的协程，将数据
	// 传递给这个协程，并将该协程置为ready状态（放入run quque，进而得到调度），
	// 然后解锁，返回值为true。
	if sg := c.recvq.dequeue(); sg != nil {
		// Found a waiting receiver. We pass the value we want to send
		// directly to the receiver, bypassing the channel buffer (if any).
		send(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true
	}

	if c.qcount < c.dataqsiz {
		// 接收等待队列肯定为空。
		//
		// 缓冲区有剩余空间，在这里无缓冲通道被视为没有剩余空间。
		// 就将数据最佳到缓冲区中，相应地移动sendx，增加qcount
		// 然后解锁，返回值为true。
		//
		// Space is available in the channel buffer. Enqueue the element to send.
		// qp是buf中c.sendx 下标元素的地址
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			racenotify(c, c.sendx, nil)
		}
		// 将待发送的数据复制到qp指向的位置。
		typedmemmove(c.elemtype, qp, ep)
		// 更新下一个写入位置
		c.sendx++
		if c.sendx == c.dataqsiz {
			// 下标越界，重置为0
			c.sendx = 0
		}
		// 递增缓冲中已存元素的计数
		c.qcount++
		unlock(&c.lock)
		return true
	}

	// 到这里表明通道已满，如果block为false，即不想阻塞，则解锁
	// 返回值为false。
	if !block {
		unlock(&c.lock)
		return false
	}
	// 至此通道已满, 等待队列为空, block 为true。
	//
	// Block on the channel. Some receiver will complete our operation for us.
	gp := getg()
	mysg := acquireSudog()
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	// elem的值是源数据的地址
	mysg.elem = ep
	mysg.waitlink = nil
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.waiting = mysg
	gp.param = nil
	// 当前协程把包含自己的 sudog 追加到通道的 sendq 排队。
	c.sendq.enqueue(mysg)
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel.
	// The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	gp.parkingOnChan.Store(true)

	// gopark() 函数挂起协程后调用 chanparkcommit() 函数对通道进行解锁，等到
	// 有接收者接收数据后，阻塞的协程会被唤醒。

	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanSend, traceEvGoBlockSend, 2)

	// Ensure the value being sent is kept alive until the
	// receiver copies it out. The sudog has a pointer to the
	// stack object, but sudogs aren't considered as roots of the
	// stack tracer.
	KeepAlive(ep)

	// someone woke us up.
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false
	closed := !mysg.success
	gp.param = nil
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	mysg.c = nil
	releaseSudog(mysg)
	if closed {
		if c.closed == 0 {
			throw("chansend: spurious wakeup")
		}
		panic(plainError("send on closed channel"))
	}
	return true
}

// send processes a send operation on an empty channel c.
// The value ep sent by the sender is copied to the receiver sg.
// The receiver is then woken up to go on its merry way.
// Channel c must be empty and locked.  send unlocks c with unlockf.
// sg must already be dequeued from c.
// ep must be non-nil and point to the heap or the caller's stack.
//
// send()，此函数当前被 chansend 和 selectgo 函数所调用，用来向通道接收队列中的等待者发送数据。
// 参数：
// c 发送数据的通道
// sg 在通道的接收队列中取出的一个等待者
// ep 要发送数据的地址。
// unlockf 解锁 c.lock
// skip 调试所用
//
// 数据传递工作室通过 sendDirect() 函数完成的，然后调用 unlockf()函数把 hchan
// 解锁，最后通过 goready() 函数唤醒接收者协程（放入任务队列）,之后返回。
//
// 因为发送数据会访问接收者协程的栈，所以 sendDirect()函数用到了写屏障。
//
func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	if raceenabled {
		if c.dataqsiz == 0 {
			racesync(c, sg)
		} else {
			// Pretend we go through the buffer, even though
			// we copy directly. Note that we need to increment
			// the head/tail locations only when raceenabled.
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
			c.recvx++
			if c.recvx == c.dataqsiz {
				c.recvx = 0
			}
			c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
		}
	}
	// 如果这个通道接收者使用了接收到的数据。
	// v:=<-c，sg.elem 的值是 &v
	if sg.elem != nil {
		// 数据复制，将 ep 指向的值复制到 sg.elem 指向的内存。
		sendDirect(c.elemtype, sg, ep)
		sg.elem = nil
	}
	gp := sg.g
	// 释放通道锁
	unlockf()
	gp.param = unsafe.Pointer(sg)
	sg.success = true
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	// 将接收等待者关联的g放入到当前P的本地任务队列。
	goready(gp, skip+1)
}

// Sends and receives on unbuffered or empty-buffered channels are the
// only operations where one running goroutine writes to the stack of
// another running goroutine. The GC assumes that stack writes only
// happen when the goroutine is running and are only done by that
// goroutine.
//
// Using a write barrier is sufficient to make up for
// violating that assumption, but the write barrier has to work.
//
// typedmemmove will call bulkBarrierPreWrite, but the target bytes
// are not in the heap, so that will not help. We arrange to call
// memmove and typeBitsBulkBarrier instead.
//
// sendDirect()，目前只被 send 函数所调用。
// 它的调用发生在，通道没有缓存或缓存队列为空且通道接收队列不为空。
//
// 参数：
// t 通道缓存的元素类型。
// sg 从通道接收队列取出的接收者。
// src 向通道发送的数据的存储地址。
// 目的：
// 将 src 指向的值复制到 sg.elem 所指向的内存。
func sendDirect(t *_type, sg *sudog, src unsafe.Pointer) {
	// src is on our stack, dst is a slot on another stack.

	// Once we read sg.elem out of sg, it will no longer
	// be updated if the destination's stack gets copied (shrunk).
	// So make sure that no preemption points can happen between read & use.
	dst := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	// No need for cgo write barrier checks because dst is always
	// Go memory.
	memmove(dst, src, t.size)
}

// recvDirect()，目前此函数只被 recv() 所调用。
// 它的调用发生在通道接收操作中，通道没有缓存且发送队列不为空的情况下。
// recvDirect() 和函数 sendDirect() 函数类似，因为要访问其它协程
// 的栈，所以在应用写屏障后进行数据复制。
// 参数：
// t 通道缓存的元素类型。
// sg 从通道发送队列取出的待唤醒的发送者。
// dst 用来从通道接收数据的变量的地址。
func recvDirect(t *_type, sg *sudog, dst unsafe.Pointer) {
	// dst is on our stack or the heap, src is on another stack.
	// The channel is locked, so src will not move during this
	// operation.
	src := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	memmove(dst, src, t.size)
}

func closechan(c *hchan) {
	if c == nil {
		panic(plainError("close of nil channel"))
	}

	lock(&c.lock)
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("close of closed channel"))
	}

	if raceenabled {
		callerpc := getcallerpc()
		racewritepc(c.raceaddr(), callerpc, abi.FuncPCABIInternal(closechan))
		racerelease(c.raceaddr())
	}

	c.closed = 1

	var glist gList

	// release all readers
	for {
		sg := c.recvq.dequeue()
		if sg == nil {
			break
		}
		if sg.elem != nil {
			typedmemclr(c.elemtype, sg.elem)
			sg.elem = nil
		}
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		glist.push(gp)
	}

	// release all writers (they will panic)
	for {
		sg := c.sendq.dequeue()
		if sg == nil {
			break
		}
		sg.elem = nil
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		glist.push(gp)
	}
	unlock(&c.lock)

	// Ready all Gs now that we've dropped the channel lock.
	for !glist.empty() {
		gp := glist.pop()
		gp.schedlink = 0
		goready(gp, 3)
	}
}

// empty reports whether a read from c would block (that is, the channel is
// empty).  It uses a single atomic read of mutable state.
//
// 通道是空的（无缓冲且sendq为空，或者通道有缓存且缓存区为空）
func empty(c *hchan) bool {
	// c.dataqsiz is immutable.
	if c.dataqsiz == 0 {
		return atomic.Loadp(unsafe.Pointer(&c.sendq.first)) == nil
	}
	return atomic.Loaduint(&c.qcount) == 0
}

// entry points for <- c from compiled code
//
// chanrecv1()
// v:=make(chan int)，会被编译成此函数的调用。
//
// channel的常规recv操作会被编译器转换为对 runtime.chanrecv1()函数的调用
// 后者内部只是调用了 runtime.chanrecv()函数。
// comma ok写法会被编器转换为对 runtime.chanrecv2()函数的调用，内部也是调用
// chanrecv()函数只不过比 chanrecv1 多了一个返回值。
//
// 非阻塞式的recv操作会编器转换为对 runtime.selectnbrec() 函数或 runtime.selectnbrecv2()
// 函数的调用（根据是否 comma ok），后两者也仅仅调用了 runtime.chanrecv()函数。
//go:nosplit
func chanrecv1(c *hchan, elem unsafe.Pointer) {
	chanrecv(c, elem, true)
}
// chanrecv2()
// v,ok:=make(chan int)操作会被编译成对此函数的调用。
//go:nosplit
func chanrecv2(c *hchan, elem unsafe.Pointer) (received bool) {
	_, received = chanrecv(c, elem, true)
	return
}

// chanrecv receives on channel c and writes the received data to ep.
// ep may be nil, in which case received data is ignored.
//
// If block == false and no elements are available, returns (false, false).
// Otherwise, if c is closed, zeros *ep and returns (true, false).
// Otherwise, fills in *ep with an element and returns (true, true).
//
// A non-nil ep must point to the heap or the caller's stack.
//
// chanrecv()，对于 select{chan operation:;default:;}，这样的select语句也会编译成
// 使用 block=false，来对 chanrecv()进行调用。其它情况下的接收操作参数 block 都为true。
//
// r:=<-make(chan int)，r,ok=<-make(chan int),会被编译成对这个函数的调用。
//
// 参数：
// c 是一个hcan指针，指向要从中recv数据的channel。
// ep 是一个指针，指向用来接收数据的内存，数据类型和要和c的元素类型一致。
// block 表示如果recv操作不能立即完成，是否想要阻塞等待。
//
// 返回值：
// selected 为true表示操作完成（可能因为通道关闭），false表示目前不能立即完成recv，但因为不想
// 阻塞（block 为false）而返回。 [判定接收到的数据是否来自于通道还是因为非阻塞取类型零值]
//
// recvived 为true表示数据确实是从通道中接收的，不是因为通道关闭而得到的零值
// 为false的情况要结合selected来解释，可能是因为通道关闭而得到零值(selected为true)
// ，或者因为不想阻塞而返回(selected为false)。
//
// [判定接收到的是不是零值(通道关闭而产生的零值)]
//
// selected   recvived
// ---selected true 表示操作操作完成（可能因为通道关闭）-----
//  true       fasle     通道关闭返回零值 block 可能为true或者false
//  true       true      从通道中接收到数据且不是零值（通道可能已经关闭或者未关闭） block可以为true或者false
// ---selected false 表示目前不能立即完成recv，但因为不想阻塞(block为false)而返回---
//  false      false     通道未关闭，非阻塞返回。 block为true。
//  false      true      不会出现这种情况。因为selected为false表示操作不能立即完成，所以recived肯定为false
// 且没有任何意义。
//
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
	// raceenabled: don't need to check ep, as it is always on the stack
	// or is new memory allocated by reflect.

	if debugChan {
		print("chanrecv: chan=", c, "\n")
	}

	if c == nil {
		if !block {
			// 非阻塞通道位nil，可以直接返回 false false
			return
		}
		// 从nil通道接收数据永久阻塞。
		gopark(nil, nil, waitReasonChanReceiveNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	//
	// 本步判断是在不加锁的情况下进行的，目的是让非阻塞recv在无法立即完成时能真正不阻塞
	// 加锁可能会阻塞。
	// empty(c) 为 single word read，所以可以保证完整性。见 chansend 函数。
	if !block && empty(c) {
		// 通道是空的（1. 无缓冲且sendq为空，2. 通道有缓存且缓存区为空，sendq为空）
		//
		// After observing that the channel is not ready for receiving, we observe whether the
		// channel is closed.
		//
		// Reordering of these checks could lead to incorrect behavior when racing with a close.
		//
		// For example, if the channel was open and not empty, was closed, and then drained,
		// reordered reads could incorrectly indicate "open and empty".
		//
		// 但同样在 empty 到下面的close之间也可能存在新到来的数据使empty为false。
		//
		// 不过有一点不同我们从closed状态的channel接收数据不必上锁，因为closed状
		// 态的channel不会再打开，也不会再添加数据。一旦判定empy和closed可以直接
		// 返回零值。
		//
		// 也就是我们可以错过队列伪空但不应该错误通道已经关闭，对于非阻塞recv来讲。
		// 因为只能选择其一。
		//
		// To prevent reordering,
		// we use atomic loads for both checks, and rely on emptying and closing to happen in
		// separate critical sections under the same lock.
		//
		// This assumption fails when closing
		// an unbuffered channel with a blocked send, but that is an error condition anyway.
		//
		// 判定通道是否关闭
		if atomic.Load(&c.closed) == 0 {
			// 如果未关闭，则直接返回两个false，因为不想阻塞而返回。
			//
			// Because a channel cannot be reopened, the later observation of the channel
			// being not closed implies that it was also not closed at the moment of the
			// first observation. We behave as if we observed the channel at that moment
			// and report that the receive cannot proceed.
			return
		}
		// The channel is irreversibly closed. Re-check whether the channel has any pending data
		// to receive, which could have arrived between the empty and closed checks above.
		// Sequential consistency is also required here, when racing with such a send.
		// 在上面的 empty(c) 到 close 直接可能有新数据的到达。
		if empty(c) {
			// 至此：通道数据队列肯定为空且通道已经关闭。
			//
			// The channel is irreversibly closed and empty.
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			// 已经关闭，就先把ep清空，然后返回true和false
			// 表名因通道关闭而得到零值。
			if ep != nil {
				typedmemclr(c.elemtype, ep)
			}
			return true, false
		}
	}

	// 至此：大概率为：通道未关闭，通道不为空，因为前面的操作并未在锁的保护下进行的。
	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)


	if c.closed != 0 {
		// 通道关闭
		if c.qcount == 0 {
			// 至此: 通道肯定关闭了且缓冲内没有数据
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			// 至此通道肯定没有数据且已经关闭，因为关闭的通道不会再填充数据，关闭的通道不能
			// 再打开。
			//
			// 可以解锁了，下面的操作不需要锁保护。
			unlock(&c.lock)
			if ep != nil {
				// 给ep赋零值，保存接收数据的变量的地址为ep。
				typedmemclr(c.elemtype, ep)
			}
			return true, false
		}
		// The channel has been closed, but the channel's buffer have data.
	} else {
		// 至此：通道肯定未关闭。
		// 如果sendq不为空，就从中取出第一个排队的协程sg
		// Just found waiting sender with not closed.
		if sg := c.sendq.dequeue(); sg != nil {
			// 至此：通道未关闭、缓存区满或没有缓冲区。
			//
			// ep的赋值要根据通道是否有缓冲，如果有这从缓冲中复制，并将sg的数据追加到
			// 缓冲。如果没有缓冲，就直接从sg那里复制。
			//
			// Found a waiting sender. If buffer is size 0, receive value
			// directly from sender. Otherwise, receive from head of queue
			// and add sender's value to the tail of the queue (both map to
			// the same buffer slot because the queue is full).
			recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
			return true, true
		}
	}

	// 如果有缓存，则还需要
	// 滚动缓冲区，完成数据的读取，并将协程sg置为ready状态（放入run queue,
	// 进而得到调度），然后解锁。这些工作都由recv()函数完成。o

	// 至此: 会有一下三种情况之一：
	// 1. 通道关闭，缓冲区有数据，发送队列为空。
	// 2. 通道未关闭，缓冲区有数据，发送队列为空。
	// 3. 通道未关闭，缓冲区没有数据，发送队列为空。
	if c.qcount > 0 {
		// 下面的逻辑将以下两种情况都处理了。
		// 1. 通道关闭，缓冲区有数据，发送队列为空。
		// 2. 通道未关闭，缓冲区有数据，发送队列为空。
		//
		// 通过 qcount 判断缓冲区是否有数据，在这里无缓冲的通道被视为没有数据，因为
		// 到达这一步sendq一定为空。如果缓存区中有数据，将第一个数据取出来并赋值给ep
		// ，移动recvx，递减qcount，解锁，返回两个true。
		//
		// Receive directly from queue
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
		}
		if ep != nil {
			// 将缓冲区的数据复制到ep所指之处。
			typedmemmove(c.elemtype, ep, qp)
		}
		// 值被赋值之后，将缓存区中对应位置清零。
		typedmemclr(c.elemtype, qp)
		// 移动 recvx，指向下一个要被接收元素的索引
		c.recvx++
		if c.recvx == c.dataqsiz {
			// 如果越界，重置到0
			c.recvx = 0
		}
		// 更新缓存区元素计数。
		c.qcount--
		unlock(&c.lock)
		// 未发生阻塞，接收到非零值。
		return true, true
	}
	// 至此：
	// 3. 通道未关闭，缓冲区没有数据，发送队列为空。
	if !block {
	// 不想阻塞，则解锁返回两个false。
		unlock(&c.lock)
		return false, false
	}

	// no sender available: block on this channel.
	// 至此：
	//    1. 通道未关闭，缓冲区没有数据(大概率，以外已经解锁了)，发送队列为空(大概率，因为已经解锁了)。
	//    2. 参数block 为true。
	gp := getg()
	mysg := acquireSudog()
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	//
	// 唤醒者负责向elem指向的位置填充数据。
	//
	mysg.elem = ep
	mysg.waitlink = nil
	gp.waiting = mysg
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.param = nil
	//
	// 入等待队列
	c.recvq.enqueue(mysg)
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel.
	//
	// The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	//
	gp.parkingOnChan.Store(true)
	//
	// gopark() 函数会在挂起当前协程后调用 chanparkcommit() 函数解锁，等到后续
	// recv操作完成时协程会被唤醒。
	//
	// 通道未关闭，缓冲区没有数据，发送队列为空。
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanReceive, traceEvGoBlockRecv, 2)

	// someone woke us up
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}

	gp.waiting = nil
	gp.activeStackChans = false
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}


	// 被唤醒可能是因为通道关闭，所以最后的返回值received需要根据别唤醒的原因来判断:
	// 1. 若是因为等到真实的数据，则为true(由chansend负责负责赋值)，
	// 2. 若是因为通道关闭，则为false(由chanclose负责赋值)。
	success := mysg.success
	gp.param = nil
	mysg.c = nil
	// 释放 sudog 结构
	releaseSudog(mysg)
	return true, success
}

// recv processes a receive operation on a full channel c.
// There are 2 parts:
//  1. The value sent by the sender sg is put into the channel
//     and the sender is woken up to go on its merry way.
//  2. The value received by the receiver (the current G) is
//     written to ep.
//
// For synchronous channels, both values are the same.
// For asynchronous channels, the receiver gets its data from
// the channel buffer and the sender's data is put in the
// channel buffer.
// Channel c must be full and locked. recv unlocks c with unlockf.
// sg must already be dequeued from c.
// A non-nil ep must point to the heap or the caller's stack.
//
// recv()，目前由 chanrecv 和 selectgo 函数调用。
// 在通道未关闭，缓冲区满或者没有缓冲区，且发送队列不为空的情况下调用。
//
// 参数：
// c 从通道c中接收数据
// sg c 的发送队列中的队首成员。
// ep 从通道c中接收到的数据存储的位置。
// unlockof 用来解锁 c.lock。
// skip 调试所用。
func recv(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	if c.dataqsiz == 0 {
		if raceenabled {
			racesync(c, sg)
		}
		if ep != nil {
			// 如果无缓存则直接通过 recvDirect() 函数进行数据赋值。
			// 不用操作缓冲区。
			// copy data from sender
			recvDirect(c.elemtype, sg, ep)
		}
	} else {
		// 若有缓存，则隐含了缓冲区已满，这样 sendq才会不为空，
		// 此时需要对缓存区进行滚动，把缓冲区头的数据取出来并接收。
		// 然后把 sendq头部的协程要发送的数据追加到缓冲区尾部。
		// 最后，通过goready()函数唤醒发送者协程就可以了。
		//
		// Queue is full. Take the item at the
		// head of the queue. Make the sender enqueue
		// its item at the tail of the queue. Since the
		// queue is full, those are both the same slot.
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
		}
		// copy data from queue to receiver
		if ep != nil {
			// 如果存在接收变量，这将缓冲区中队首元素复制到其中。
			typedmemmove(c.elemtype, ep, qp)
		}
		// copy data from sender to queue
		//
		// 再用发送者所发送的数据去填充。
		typedmemmove(c.elemtype, qp, sg.elem)
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		// 因为缓存区是满的，当 c.recvx更新后，c.sendx 必须更新为一样的值。
		c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
	}
	sg.elem = nil
	gp := sg.g
	// 解锁 c.lock
	unlockf()
	gp.param = unsafe.Pointer(sg)
	sg.success = true
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	// 将其存储到当前p的runnext字段后返回。
	goready(gp, skip+1)
}

func chanparkcommit(gp *g, chanLock unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	// Set activeStackChans here instead of before we try parking
	// because we could self-deadlock in stack growth on the
	// channel lock.
	gp.activeStackChans = true
	// Mark that it's safe for stack shrinking to occur now,
	// because any thread acquiring this G's stack for shrinking
	// is guaranteed to observe activeStackChans after this store.
	gp.parkingOnChan.Store(false)

	// Make sure we unlock after setting activeStackChans and
	// unsetting parkingOnChan.
	// The moment we unlock chanLock
	// we risk gp getting readied by a channel operation and
	// so gp could continue running before everything before the unlock is visible (even to gp itself).
	unlock((*mutex)(chanLock))
	return true
}

// compiler implements
//
//	select {
//	case c <- v:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbsend(c, v) {
//		... foo
//	} else {
//		... bar
//	}
// 非阻塞式的send操作会被编译器转换为对 runtime.selectnbsend()函数的
// 调用，后者也仅仅调用了 runtime.chansend()函数。
func selectnbsend(c *hchan, elem unsafe.Pointer) (selected bool) {
	return chansend(c, elem, false, getcallerpc())
}

// compiler implements
//
//	select {
//	case v, ok = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selected, ok = selectnbrecv(&v, c); selected {
//		... foo
//	} else {
//		... bar
//	}
// 非阻塞式的send操作会被编译器转换为对 runtime.selectnbrecv()函数的
// 调用，后者也仅仅调用了 runtime.chanrecv()函数。
func selectnbrecv(elem unsafe.Pointer, c *hchan) (selected, received bool) {
	return chanrecv(c, elem, false)
}

//go:linkname reflect_chansend reflect.chansend
func reflect_chansend(c *hchan, elem unsafe.Pointer, nb bool) (selected bool) {
	return chansend(c, elem, !nb, getcallerpc())
}

//go:linkname reflect_chanrecv reflect.chanrecv
func reflect_chanrecv(c *hchan, nb bool, elem unsafe.Pointer) (selected bool, received bool) {
	return chanrecv(c, elem, !nb)
}

//go:linkname reflect_chanlen reflect.chanlen
func reflect_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflectlite_chanlen internal/reflectlite.chanlen
func reflectlite_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflect_chancap reflect.chancap
func reflect_chancap(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.dataqsiz)
}

//go:linkname reflect_chanclose reflect.chanclose
func reflect_chanclose(c *hchan) {
	closechan(c)
}

func (q *waitq) enqueue(sgp *sudog) {
	sgp.next = nil
	x := q.last
	if x == nil {
		sgp.prev = nil
		q.first = sgp
		q.last = sgp
		return
	}
	sgp.prev = x
	x.next = sgp
	q.last = sgp
}

func (q *waitq) dequeue() *sudog {
	for {
		sgp := q.first
		if sgp == nil {
			return nil
		}
		y := sgp.next
		if y == nil {
			q.first = nil
			q.last = nil
		} else {
			y.prev = nil
			q.first = y
			sgp.next = nil // mark as removed (see dequeueSudoG)
		}

		// if a goroutine was put on this queue because of a
		// select, there is a small window between the goroutine
		// being woken up by a different case and it grabbing the
		// channel locks. Once it has the lock
		// it removes itself from the queue, so we won't see it after that.
		// We use a flag in the G struct to tell us when someone
		// else has won the race to signal this goroutine but the goroutine
		// hasn't removed itself from the queue yet.
		//
		// select多路选择中，如果在轮询中发现没有任何case操作可以立即完成，则会进入第二
		// 阶段，按照上锁的顺序为每个case创建一个关联当前g的sudog，并将其放入到case关联
		// 通道的sendq或者recvq队列。
		// 然后调用gopark将自己挂起，并在系统栈中释放select中涉及到的所有通道的锁。
		// 因为select中涉及的不止一个通道，这样当不只一个case就绪时，就会在获取关联g的sudog
		// 上存在竞争。因为关联g只能被唤醒一次，但它却被多个sudog引用，所以下面通过对
		// g的selectDone进行cas操作，这保证了g只会被唤醒一次。
		// 又因为当g将g.selectDone重置时其已经从各个case关联通道的相应等待队列中移除了，
		// 所以没有任何问题。
		//
		if sgp.isSelect && !sgp.g.selectDone.CompareAndSwap(0, 1) {
			continue
		}

		return sgp
	}
}

func (c *hchan) raceaddr() unsafe.Pointer {
	// Treat read-like and write-like operations on the channel to
	// happen at this address. Avoid using the address of qcount
	// or dataqsiz, because the len() and cap() builtins read
	// those addresses, and we don't want them racing with
	// operations like close().
	return unsafe.Pointer(&c.buf)
}

func racesync(c *hchan, sg *sudog) {
	racerelease(chanbuf(c, 0))
	raceacquireg(sg.g, chanbuf(c, 0))
	racereleaseg(sg.g, chanbuf(c, 0))
	raceacquire(chanbuf(c, 0))
}

// Notify the race detector of a send or receive involving buffer entry idx
// and a channel c or its communicating partner sg.
// This function handles the special case of c.elemsize==0.
func racenotify(c *hchan, idx uint, sg *sudog) {
	// We could have passed the unsafe.Pointer corresponding to entry idx
	// instead of idx itself.  However, in a future version of this function,
	// we can use idx to better handle the case of elemsize==0.
	// A future improvement to the detector is to call TSan with c and idx:
	// this way, Go will continue to not allocating buffer entries for channels
	// of elemsize==0, yet the race detector can be made to handle multiple
	// sync objects underneath the hood (one sync object per idx)
	qp := chanbuf(c, idx)
	// When elemsize==0, we don't allocate a full buffer for the channel.
	// Instead of individual buffer entries, the race detector uses the
	// c.buf as the only buffer entry.  This simplification prevents us from
	// following the memory model's happens-before rules (rules that are
	// implemented in racereleaseacquire).  Instead, we accumulate happens-before
	// information in the synchronization object associated with c.buf.
	if c.elemsize == 0 {
		if sg == nil {
			raceacquire(qp)
			racerelease(qp)
		} else {
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
		}
	} else {
		if sg == nil {
			racereleaseacquire(qp)
		} else {
			racereleaseacquireg(sg.g, qp)
		}
	}
}
