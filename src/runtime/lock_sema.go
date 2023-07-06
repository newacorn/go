// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || netbsd || openbsd || plan9 || solaris || windows

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// This implementation depends on OS-specific implementations of
//
//	func semacreate(mp *m)
//		Create a semaphore for mp, if it does not already have one.
//
//	func semasleep(ns int64) int32
//		If ns < 0, acquire m's semaphore and return 0.
//		If ns >= 0, try to acquire m's semaphore for at most ns nanoseconds.
//		Return 0 if the semaphore was acquired, -1 if interrupted or timed out.
//
//	func semawakeup(mp *m)
//		Wake up mp, which is or will soon be sleeping on its semaphore.
const (
	locked uintptr = 1

	active_spin     = 4
	active_spin_cnt = 30
	passive_spin    = 1
)

func lock(l *mutex) {
	lockWithRank(l, getLockRank(l))
}

func lock2(l *mutex) {
	gp := getg()
	if gp.m.locks < 0 {
		throw("runtime·lock: lock count")
	}
	gp.m.locks++

	// Speculative grab for lock.
	if atomic.Casuintptr(&l.key, 0, locked) {
		return
	}
	// semacreate() 函数:如果信号量未初始化执行初始化，否则直接返回。
	semacreate(gp.m)

	// On uniprocessor's, no point spinning.
	// On multiprocessors, spin for ACTIVE_SPIN attempts.
	spin := 0
	if ncpu > 1 {
		// active_spin 的值是常量4
		// 单核cpu自旋没有意义
		spin = active_spin
	}
Loop:
	for i := 0; ; i++ {
		v := atomic.Loaduintptr(&l.key)
		if v&locked == 0 {
			// Unlocked. Try to lock.
			if atomic.Casuintptr(&l.key, v, v|locked) {
				return
			}
			i = 0
		}
		if i < spin {
			//i<4 (多核cpu)
			// 循环自旋30次PAUSE
			// 不会让线程休眠
			procyield(active_spin_cnt)
		} else if i < spin+passive_spin {
			// i<5
			// 通过自旋系统调用来切换至其它线程
			osyield()
		} else {
			// 尝试获取锁失败
			// Someone else has it.
			// l->waitm points to a linked list of M's waiting
			// for this lock, chained through m->nextwaitm.
			// Queue this M.
			for {
				gp.m.nextwaitm = muintptr(v &^ locked)
				if atomic.Casuintptr(&l.key, v, uintptr(unsafe.Pointer(gp.m))|locked) {
					// 形成 waitm 链成功过
					break
				}
				v = atomic.Loaduintptr(&l.key)
				// 再次尝试
				if v&locked == 0 {
					// 如果l.key现在未加锁，便执行大循环尝试获取锁，而不是形成 waitm 链。
					continue Loop
				}
			}
			// 再次尝试,看是否上锁了,如果未上锁继续循环可能会获得锁。
			// 但是上面已经将当期那M添加到了waitm列表，M并不一定在休眠等待锁。
			//
			// 其实这里的检查到未上锁，证明其它线程在 v&locked=0之前已经从waitm链去除了
			// 此M，所以不会存在M获得锁但其还在 waitm链中。
			if v&locked != 0 {
				// Queued. Wait.
				// 线程休眠
				semasleep(-1)
				i = 0
			}
		}
	}
}

func unlock(l *mutex) {
	unlockWithRank(l)
}

// We might not be holding a p in this code.
//
//go:nowritebarrier
func unlock2(l *mutex) {
	gp := getg()
	var mp *m
	for {
		v := atomic.Loaduintptr(&l.key)
		if v == locked {
			// 有可能在此期间，另一个M因为获得不到锁而构成一个阻塞
			// 队列并将首M的地址按位与到了l.key中。所以l.key不再等于locked。
			// 这种情况下只能继续循环。
			if atomic.Casuintptr(&l.key, locked, 0) {
				// 没有处于休眠状态的等待线程，直接尝试解锁。
				break
			}
		} else {
			// Other M's are waiting for the lock.
			// Dequeue an M.
			mp = muintptr(v &^ locked).ptr()
			    // 下面的if语句不一定执行成功，如果在下面的语句执行之前，又有一个m
				// 插入了等待链表。这样 *l.key 与 v便不再相等。
				// 这种情况下便只能继续循环。
			if atomic.Casuintptr(&l.key, v, uintptr(mp.nextwaitm)) {
				// 下面唤醒一个mp去尝试获取该锁，但是这个mp未必一定能获得该锁
				// 有可能其它线程在mp恢复执行时已经获得了锁。又或者它们同时运行
				// 但mp抢锁失败。
				//
				// 这一点不同于linux的 utex机制，即在有等待线程的情况下，解锁
				// 者未必一定会唤醒一个等待线程。
				// 而基于sema机制的实现肯定会，糟糕的是这个被唤醒的线程不一定可以
				// 获得想要的锁，这样看来futex效率更高。
				//
				// futex的缺点是，一个执行解锁的线程发现l.key=mutex_sleeping时
				// 执行唤醒操作，可能根本没有一个等待者，即这个解锁操作是个空操作。
				//
				// Dequeued an M.  Wake it.
				semawakeup(mp)
				break
			}
		}
	}
	gp.m.locks--
	if gp.m.locks < 0 {
		throw("runtime·unlock: lock count")
	}
	if gp.m.locks == 0 && gp.preempt { // restore the preemption request in case we've cleared it in newstack
		gp.stackguard0 = stackPreempt
	}
}

// One-time notifications.
// 清空note.key 状态，之后可以复用note。
// 一般只能在 notesleep 返回之后调用。
func noteclear(n *note) {
	if GOOS == "aix" {
		// On AIX, semaphores might not synchronize the memory in some
		// rare cases. See issue #30189.
		atomic.Storeuintptr(&n.key, 0)
	} else {
		n.key = 0
	}
}
// 唤醒线程M
func notewakeup(n *note) {
	var v uintptr
	for {
		v = atomic.Loaduintptr(&n.key)
		if atomic.Casuintptr(&n.key, v, locked) {
			break
		}
	}

	// Successfully set waitm to locked.
	// What was it before?
	switch {
	case v == 0:
		// Nothing was waiting. Done.
	case v == locked:
		// Two notewakeups! Not allowed.
		throw("notewakeup - double wakeup")
	default:
		// Must be the waiting m. Wake it up.
		semawakeup((*m)(unsafe.Pointer(v)))
	}
}
// notesleep()，在g0栈上调用。
//
// 1. 休眠线程M，等待唤醒。
// 对于特定的 note，只能有一个线程调用 notesleep 进入休眠，
// 另外一个线程调用 notewakeup 唤醒， notesleep 会返回。
// 要想复用 note ，需在 notesleep 返回后调用 noteclear。
//
// 2. notewakeup 调用后， notesleep 唤醒返回后再接着调用 notesleep 会直接返回。
// notewakeup 调用之后，在没有调用过 noteclear 继续调用 notewakeup 会抛出异常。
//
// see: notesleepg 在用户g栈上调用。
// notesleep on g0
func notesleep(n *note) {
	gp := getg()
	if gp != gp.m.g0 {
		throw("notesleep not on g0")
	}
	// 初始化信号量
	semacreate(gp.m)
	if !atomic.Casuintptr(&n.key, 0, uintptr(unsafe.Pointer(gp.m))) {
		// 信号量已经就绪，note信号量就绪的标志是n.key=loced
		// Must be locked (got wakeup).
		if n.key != locked {
			throw("notesleep - waitm out of sync")
		}
		return
	}
	// Queued. Sleep.
	// 信号量未就绪休眠线程
	gp.m.blocked = true
	if *cgo_yield == nil {
		// 进入休眠等待信号量就绪
		semasleep(-1)
	} else {
		// Sleep for an arbitrary-but-moderate interval to poll libc interceptors.
		const ns = 10e6
		for atomic.Loaduintptr(&n.key) == 0 {
			semasleep(ns)
			asmcgocall(*cgo_yield, nil)
		}
	}
	gp.m.blocked = false
}
// notetsleep_internal(),目前在 notetsleep 和 notetsleepg 函数中调用。
// 带超时的方式使当前线程进入休眠。唤醒函数为 notewakeup()
//
// 参数：
// ns: 当ns小于0时会永久休眠,直到另外一个线程唤醒。大于等于0时，唤醒机制多了个超时唤醒。
// deadline: 这个参数没有用到，函数中的 deadline = now()+ns。
//
// 返回值：
// true 信号量就绪
// false 信号量未就绪超时，且&m已经从 n.key 上移除了，且n.key设置为0。
//
//go:nosplit
func notetsleep_internal(n *note, ns int64, gp *g, deadline int64) bool {
	// gp and deadline are logically local variables, but they are written
	// as parameters so that the stack space they require is charged
	// to the caller.
	// This reduces the nosplit footprint of notetsleep_internal.
	// gp存储在调用者栈空间。
	gp = getg()

	// Register for wakeup on n->waitm.
	if !atomic.Casuintptr(&n.key, 0, uintptr(unsafe.Pointer(gp.m))) {
		// 信号量已经就绪了,直接返回
		// Must be locked (got wakeup).
		if n.key != locked {
			throw("notetsleep - waitm out of sync")
		}
		return true
	}
	// n.key 现在存储的是m的地址了。
	if ns < 0 {
		// ns小于0，线程m永久阻塞,等待唤醒
		// Queued. Sleep.
		gp.m.blocked = true
		if *cgo_yield == nil {
			semasleep(-1)
		} else {
			// Sleep in arbitrary-but-moderate intervals to poll libc interceptors.
			const ns = 10e6
			for semasleep(ns) < 0 {
				asmcgocall(*cgo_yield, nil)
			}
		}
		gp.m.blocked = false
		return true
	}

	deadline = nanotime() + ns
	for {
		// Registered. Sleep.
		gp.m.blocked = true
		if *cgo_yield != nil && ns > 10e6 {
			// ns > 1ms
			// 每次休眠1ms。
			ns = 10e6
		}
		if semasleep(ns) >= 0 {
			// semasleep(ns) 返回值大于等于0，表示信号量已经就绪
			// 可以返回了。
			gp.m.blocked = false
			// Acquired semaphore, semawakeup unregistered us.
			// Done.
			return true
		}
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		gp.m.blocked = false
		// Interrupted or timed out. Still registered. Semaphore not acquired.
		ns = deadline - nanotime()
		// ns最小为-1ms，即必预计超时多休眠1ms才返回。
		if ns <= 0 {
			// 超时，信号量还未就绪跳出循环
			break
		}
		// Deadline hasn't arrived. Keep sleeping.
	}

	// Deadline 时间已经到了，但信号量还未就绪。
	//
	// Deadline arrived. Still registered. Semaphore not acquired.
	// Want to give up and return, but have to unregister first,
	// so that any notewakeup racing with the return does not
	// try to grant us the semaphore when we don't expect it.
	for {
		v := atomic.Loaduintptr(&n.key)
		switch v {
		case uintptr(unsafe.Pointer(gp.m)):
			// No wakeup yet; unregister if possible.
			// 尝试取消等待信号量
			// 不一定会成功，如果此时另外一个线程调用了 notewakeup
			// n.key 就不会再等于上面的v了。
			if atomic.Casuintptr(&n.key, v, 0) {
				// 取消成功返回false
				return false
			}
		case locked:
			// 信号量就绪，说明已经调用过了 notewakeup 函数了。
			// Wakeup happened so semaphore is available.
			// Grab it to avoid getting out of sync.
			gp.m.blocked = true
			// 一致性校验，当信号量就绪时 semasleep 会直接返回0
			// 调用 notewakeup 的线程会调用 seamwakeup，让底层
			// 信号量就绪。
			if semasleep(-1) < 0 {
				throw("runtime: unable to acquire - semaphore out of sync")
			}
			gp.m.blocked = false
			return true
		default:
			// 上面两个case肯定会成功一个，对应的是要么另外一个线程调用了 notewakeup，要么
			// 没有调用。
			throw("runtime: unexpected waitm - semaphore out of sync")
		}
	}
}
// notetsleep()
//
// 与 notesleep() 相同都在在g0栈上调用，不同的是其可以带一个休眠超时
// 参数，在多少纳秒后自动唤醒。
//
// 参数：
// n: 包含信号量的结构体
// ns: 休眠超时(纳秒)
// 返回值：
// true: 表示被其它线程唤醒而不是超时唤醒。
// false: 超时唤醒。
//
func notetsleep(n *note, ns int64) bool {
	gp := getg()
	if gp != gp.m.g0 {
		throw("notetsleep not on g0")
	}
	semacreate(gp.m)
	return notetsleep_internal(n, ns, nil, 0)
}

// same as runtime·notetsleep, but called on user g (not g0)
// calls only nosplit functions between entersyscallblock/exitsyscall
//
// 线程休眠但会释放关联的P，相当于系统调用。返回后如果没有获取P，则当前协程会放入全局队列，
// 休眠线程。
// notetsleep 、 notesleep 都不会释放线程关联P，也不会引发协程调度。
func notetsleepg(n *note, ns int64) bool {
	gp := getg()
	if gp == gp.m.g0 {
		throw("notetsleepg on g0")
	}
	semacreate(gp.m)
	// 释放关联p
	entersyscallblock()
	ok := notetsleep_internal(n, ns, nil, 0)
	// 尝试获取p，继续执行。
	exitsyscall()
	return ok
}

func beforeIdle(int64, int64) (*g, bool) {
	return nil, false
}

func checkTimeouts() {}
