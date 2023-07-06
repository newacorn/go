// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dragonfly || freebsd || linux

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// This implementation depends on OS-specific implementations of
//
//	futexsleep(addr *uint32, val uint32, ns int64)
//		Atomically,
//			if *addr == val { sleep }
//		Might be woken up spuriously; that's allowed.
//		Don't sleep longer than ns; ns < 0 means forever.
//
//	futexwakeup(addr *uint32, cnt uint32)
//		If any procs are sleeping on addr, wake up at most cnt.

const (
	// 未上锁。
	mutex_unlocked = 0
	// 有锁但没有等待者。
	mutex_locked   = 1
	// 有锁但有等待者。
	mutex_sleeping = 2

	// 主动自旋，当多核时才被使用。
	active_spin     = 4
	// 主动自旋中每次循环步执行30次PAUSE指令。
	active_spin_cnt = 30
	// 被动自旋次数，在循环步中会让出线程占用的时间片，切换到其他线程执行。
	passive_spin    = 1
)

// Possible lock states are mutex_unlocked, mutex_locked and mutex_sleeping.
// mutex_sleeping means that there is presumably at least one sleeping thread.
// Note that there can be spinning threads during all states - they do not
// affect mutex's state.

// We use the uintptr mutex.key and note.key as a uint32.
//
//go:nosplit
func key32(p *uintptr) *uint32 {
	return (*uint32)(unsafe.Pointer(p))
}

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
	//
	// 直接用 mutex_locked 替换旧值，不管旧值是什么。
	// 当此调用返回时(即成功获取锁)，l.key 中存储的要
	// 么是 mutex_locked，要么是 mutex_sleeping。
	//
	// 这样都符合逻辑，但如果存储的是 mutex_sleeping，
	// 可能并没有真的等待者，会导致一次没有效果的 futexwakeup
	// 调用，不过没什么问题。
	v := atomic.Xchg(key32(&l.key), mutex_locked)
	if v == mutex_unlocked {
		// 获得锁成功直接返回。
		//
		// 不过此时可能有等待者：
		// 另一个线程释放锁时会在l.key中存储 mutex_unlocked，并保存旧值。
		// 此时这个函数执行可以到达此逻辑。
		// 旧值肯定是mutex_sleeping,解锁的那个线程会调用一次唤醒线程操作。
		// 但是上面的唤醒线程操作执行时，l.key中存储的是 mutex_unlocked，
		// 此线程解锁时被唤醒的线程分两种情况：
		// 1. 在此线程解锁之后，它才开始运行这样它便能获得该锁，并将l.key设置为wait值，
		//    又因线程在唤醒时wait值都是 mutex_sleeping，在这个被唤醒线程获得锁之后
		//    即使有其它休眠线程也会被这个线程唤醒。
		// 2. 在此线程解锁之前，它已经完成运行进入了休眠，又知线程在被唤醒后进入休眠前会
		//    将l.key设置为wait的值，所以l.key为mutex_sleeping。此线程解锁时发现l.key
		//    的状态为 mutex_sleeping 时会执行唤醒逻辑。
		//  当然还有其他情况。
		//  但是仔细推敲后会发现任何一种情况都不会有问题。
		return
	}

	// wait is either MUTEX_LOCKED or MUTEX_SLEEPING
	// depending on whether there is a thread sleeping
	// on this mutex. If we ever change l->key from
	// MUTEX_SLEEPING to some other value, we must be
	// careful to change it back to MUTEX_SLEEPING before
	// returning, to ensure that the sleeping thread gets
	// its wakeup call.
	//
	// wait 保存旧值，这个很重要。特别是当旧值为 mutex_sleeping时。
	// 其实将所有旧值都设置为 mutex_sleeping 也不会有错，只是会多
	// 执行些无用的唤醒线程操作。但是不会造成死锁即不会漏掉待唤醒者。
	//
	// 执行到这里 wait肯定是 mutex_sleeping 右或者是 mutex_locked
	wait := v

	// On uniprocessors, no point spinning.
	// On multiprocessors, spin for ACTIVE_SPIN attempts.
	spin := 0
	// 单核下执行自旋没有意义。
	if ncpu > 1 {
		// 多核下 spin=4
		spin = active_spin
	}
	for {
		// Try for lock, spinning.
		for i := 0; i < spin; i++ {
			for l.key == mutex_unlocked {
				// 如果旧值是 mutex_unlocked，说明在当前线程看来没有等待者，
				// 所以可以用 wait替换，无论wait是 mutex_locked又或者是
				// mutex_sleeping都没问题，因为两者都表示有锁，只是mutex_sleeping
				// 需要多执行一次唤醒线程的操作。
				//
				// 即使下面的Cas操作执行成功，也并不代表没有等待者，只是对当前线程不可见
				// 释放这个锁给当前线程的线程可以观察到，因为它保存了旧值。如果是mutex_sleeping
				// 它会执行唤醒调用。
				if atomic.Cas(key32(&l.key), mutex_unlocked, wait) {
					return
				}
			}
			procyield(active_spin_cnt)
		}

		// Try for lock, rescheduling.
		for i := 0; i < passive_spin; i++ {
			for l.key == mutex_unlocked {
				// 在此尝试，情况同上。
				if atomic.Cas(key32(&l.key), mutex_unlocked, wait) {
					return
				}
			}
			// 让出线程时间片，给其他线程执行机会。
			osyield()
		}

		// Sleep.
		// 告知其它线程即将有个等待者，即使当前线程不会休眠。也不会
		// 错过会休眠的情况。
		v = atomic.Xchg(key32(&l.key), mutex_sleeping)
		if v == mutex_unlocked {
			// 或取V之后，如果v是 mutex_unlocked，直到释放前肯定不会
			// 有其它线程再能获得锁了，因为上面使用 mutex_sleeping替换
			// 的，它也表示有锁。
			//
			// 再次尝试，如果成功即可返回，现在即使没有其它等待者，mutex_sleeping
			// 带来的不过是一个没有效果的解锁操作。
			return
		}
		//
		// wait 设置为 mutex_sleeping 特别重要，因为当被唤醒后
		// 需要将wait替换掉l.key中存储的值，如果替换掉的旧l.key值
		// 是mutex_unlocked，那么便成功获得了锁.但如果同时还有其它
		// 等待者了呢？所以当被唤醒者解锁时在替换l.key中的旧值时发现
		// 旧值是mutex_sleeping就会执行唤醒操作，而不会漏掉待唤醒者。
		wait = mutex_sleeping
		// 获取锁失败，当前线程进入休眠，-1表示永久休眠直到被显式唤醒。
		futexsleep(key32(&l.key), mutex_sleeping, -1)
	}
}

func unlock(l *mutex) {
	unlockWithRank(l)
}

func unlock2(l *mutex) {
	v := atomic.Xchg(key32(&l.key), mutex_unlocked)
	// 保存旧值
	if v == mutex_unlocked {
		throw("unlock of unlocked lock")
	}
	if v == mutex_sleeping {
		// 执行唤醒操作，唤醒一个等待线程。
		futexwakeup(key32(&l.key), 1)
	}

	gp := getg()
	gp.m.locks--
	if gp.m.locks < 0 {
		throw("runtime·unlock: lock count")
	}
	if gp.m.locks == 0 && gp.preempt {
		// restore the preemption request in case we've cleared it in newstack
		gp.stackguard0 = stackPreempt
	}
}

// One-time notifications.
func noteclear(n *note) {
	n.key = 0
}

func notewakeup(n *note) {
	old := atomic.Xchg(key32(&n.key), 1)
	if old != 0 {
		print("notewakeup - double wakeup (", old, ")\n")
		throw("notewakeup - double wakeup")
	}
	futexwakeup(key32(&n.key), 1)
}

func notesleep(n *note) {
	gp := getg()
	if gp != gp.m.g0 {
		throw("notesleep not on g0")
	}
	ns := int64(-1)
	if *cgo_yield != nil {
		// Sleep for an arbitrary-but-moderate interval to poll libc interceptors.
		ns = 10e6
	}
	for atomic.Load(key32(&n.key)) == 0 {
		gp.m.blocked = true
		futexsleep(key32(&n.key), 0, ns)
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		gp.m.blocked = false
	}
}

// May run with m.p==nil if called from notetsleep, so write barriers
// are not allowed.
//
//go:nosplit
//go:nowritebarrier
func notetsleep_internal(n *note, ns int64) bool {
	gp := getg()

	if ns < 0 {
		if *cgo_yield != nil {
			// Sleep for an arbitrary-but-moderate interval to poll libc interceptors.
			ns = 10e6
		}
		for atomic.Load(key32(&n.key)) == 0 {
			gp.m.blocked = true
			futexsleep(key32(&n.key), 0, ns)
			if *cgo_yield != nil {
				asmcgocall(*cgo_yield, nil)
			}
			gp.m.blocked = false
		}
		return true
	}

	if atomic.Load(key32(&n.key)) != 0 {
		return true
	}

	deadline := nanotime() + ns
	for {
		if *cgo_yield != nil && ns > 10e6 {
			ns = 10e6
		}
		gp.m.blocked = true
		futexsleep(key32(&n.key), 0, ns)
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		gp.m.blocked = false
		if atomic.Load(key32(&n.key)) != 0 {
			break
		}
		now := nanotime()
		if now >= deadline {
			break
		}
		ns = deadline - now
	}
	return atomic.Load(key32(&n.key)) != 0
}

func notetsleep(n *note, ns int64) bool {
	gp := getg()
	if gp != gp.m.g0 && gp.m.preemptoff != "" {
		throw("notetsleep not on g0")
	}

	return notetsleep_internal(n, ns)
}

// same as runtime·notetsleep, but called on user g (not g0)
// calls only nosplit functions between entersyscallblock/exitsyscall.
func notetsleepg(n *note, ns int64) bool {
	gp := getg()
	if gp == gp.m.g0 {
		throw("notetsleepg on g0")
	}

	entersyscallblock()
	ok := notetsleep_internal(n, ns)
	exitsyscall()
	return ok
}

func beforeIdle(int64, int64) (*g, bool) {
	return nil, false
}

func checkTimeouts() {}
