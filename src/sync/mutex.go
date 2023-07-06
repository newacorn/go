// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// Provided by runtime via linkname.
func throw(string)
func fatal(string)

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
//
// In the terminology of the Go memory model,
// the n'th call to Unlock “synchronizes before” the m'th call to Lock
// for any n < m.
// A successful call to TryLock is equivalent to a call to Lock.
// A failed call to TryLock does not establish any “synchronizes before”
// relation at all.
//
// 它是一把结合了自旋锁与信号量 sema 的优化过的锁。
// 它是协程相关的锁，无论如何都不会阻塞线程，因为底层的信号量机制也是协程态的。
//
// 锁的状态与协程不会绑定，一个协程执行 Lock 成功获得了锁，另一个协程在此之后可
// 以执行 Unlock 来解锁。
//
// 此结构的零值是一个有效的互斥锁，处于Unlocked状态。
//
// state 存储的是互斥锁的状态，加锁和解锁方法都是通过aomic包提供
// 的函数原子性地操作该字段。
//
// Lock 之后接着再 Lock 所在的协程会加入等待队列，不支持重入。
// Unlock 之后接着再 Unlock 会导致panic。
type Mutex struct {
	// 31 30 .............      2              1         0
	// |--等待队列规模-----|-工作模式标志位-|-唤醒标志位-|-加锁状态标志位-|
	state int32
	// 用来排队未获得锁的goroutine，
	// 和用来唤醒排队中的goroutine。
	// sema字段用作信号量，为Mutex提供等待队列。
	sema  uint32 //调用解锁之前其必为0，调用解锁时增加1。此时位于锁等待队列的G会被自动唤醒（减1）加入到runnable队列。
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	// 表示互斥锁处于 Locked 状态。
	//
	// 由或的锁的g设置。
	mutexLocked = 1 << iota // mutex is locked

	// 表示已经有 goroutine 被唤醒了，当该标志位被设置时，Unlock 操作不会唤醒排队的 goroutine。
	// 同时新入 Lock 调用的g也不会再尝试设置 mutexWoken 标志位了。
	//
	// mutexWoken 由新入 Lock 调用的g设置，或者由唤醒者设置。
	mutexWoken

	// 表示饥饿模式，该标志位被设置时 Mutex 工作在饥饿模式，清零时 Mutex 工作在正常模式。
	//
	// mutexStarving 在以下条件才有可能被设置：
	// 正常模式下被唤醒的G必须没能获得锁情况下才能将正常模式设置为饥饿模式。
	// 也就是说它必定会调用 runtime_SemacquireMutex 函数。
	// 具体缘由见：
	/**
	// Lock 调用中，设置 mutexStarving 标志位的条件：
	if starving && old&mutexLocked != 0 {
	// 这两个条件只有正常模式下被唤醒的等待者才有可能满足。
	// 1. starving 成立的条件：正常模式下被唤醒的G且在队列中已经超过了1ms。
	// 2. 唤醒者能获得锁。

	// 由此可见如果想进入饥饿模式：正常模式下被唤醒的G必须没能获得锁情况下才能
	// 将模式设置为饥饿模式。因为要设置 mutexStarving 标志位，old必须设置了
		// mutexLocked 标志位，要想将 mutexStarving 标志存储到 Mutex.state
		// 中 old又必须不能变更。所以这个将正常模式变更到饥饿模式的唤醒者必定会执行
		// runtime_SemacquireMutex(&m.sema, queueLifo, 1)。
		//
		// 不过有可能执行之后，便立即返回。因为解锁着可能会在其调用runtime_SemacquireMutex
		// 之前调用了 runtime_SemreleaseMutex。由于信号量一次release针对一次acquire，不分
		// 先后执行。
		new |= mutexStarving
	}
	 */
	//
	// mutexStarving 和 mutexWoken 是互斥的。
	// mutexStarving 如果设置了，必有等待者。
	mutexStarving
	// 表示除了最低3位以外，state的其他位用来记录有多少个等待者在排队。
	//
	// 等待者计数递增新入 Lock 调用的g，在未获得锁的情况下而进入等待队列时设置。
	// 或者被唤醒的g，未获得锁又进入等待队列而递增。
	//
	// 等待者计数的递减由解锁方在正常模式下唤醒等待者时由唤醒者帮其递减，或者饥饿模
	// 式下被唤醒的g在从 Lock 返回时设置。
	mutexWaiterShift = iota

	// 31 30 .............      2              1         0
	// |--等待队列规模-----|-工作模式标志位-|-唤醒标志位-|-加锁状态标志位-|

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	starvationThresholdNs = 1e6
)
// Lock()
// 要么获得锁成功，要么调用协程进入等待队列，等待锁的持有者唤醒。
// 肯定会做的事：
// 1. 如果休眠会将等待计数加1。
// 2. 如果返回会设置 mutexLocked 标志位。
// 可能会做的事：
// 1. 进入休眠除了增加等待计数可能还会设置饥饿模式位。
// 2. 如果返回除了会设置 mutexLocked 标志位，可能还会设置饥饿模式位。
// 3. 如果返回除了会设置 mutexLocked 标志位，可能还会取消饥饿模式位。
//
// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		// 没有等待者、锁没有被获取、工作在正常模式下。
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	m.lockSlow()
}

// TryLock tries to lock m and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (m *Mutex) TryLock() bool {
	old := m.state
	if old&(mutexLocked|mutexStarving) != 0 {
		// 能获取锁的前提条件，未满足。
		return false
	}

	// There may be a goroutine waiting for the mutex, but we are
	// running now and can try to grab the mutex before that
	// goroutine wakes up.
	if !atomic.CompareAndSwapInt32(&m.state, old, old|mutexLocked) {
		return false
	}
	// 至此: 此调用在非饥饿模式下成功地将 Mutex.state 状态由未设置 mutexLoked 变更到设置了 mutexLocked
	// 标志位。所以尝试获得锁获得锁成功。

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
	return true
}

func (m *Mutex) lockSlow() {

	// 开始等待时间戳，即从 Lock 调用开始。
	var waitStartTime int64

	// 是否符合饥饿模式的条件。
	starving := false

	// 两种情况下可为true：
	// 1. 新入 Lock 的g成功设置了mutexWoken标志位。
	// 2. 正常模式下，一个被唤醒的等待者在恢复运行时会设置此值。
	awoke := false

	// 正常模式下，一个被唤醒的等待者在别唤醒之后，会将此值重置为0。
	iter := 0 // 自旋次数

	// 存储进入循环前的状态。循环步中会在old基础之上通过设置或清除标志位，
	// 以获得目标状态new，然后通过 atomic.CompareAndSwapInt32(old,new)
	// 进行替换，如果old表示的状态在获取状态之后和执行原子切换前被更改了，则需
	// 重新获得old，再根据情况来设置new。
	// 直到atomic.CompareAndSwapInt调用成功。之后或进入等待队列，或获得锁成功返回。
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		//
		// 不处于饥饿模式下且已锁才会进入自旋：
		// 1. 因为饥饿模式下：锁的传递严格限制，只能有持有锁方传递给被它唤醒的等待者。因为在解锁
		// 之后到唤醒者设置 mutexLocked 标志位存在空窗期，所以不能有任何处于自旋状态的g。
		// 2. 如果mutex目前没有被锁，自旋没有意义，而是直接尝试获取锁。
		//
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// 至此：g为新入 Lock 的g或被唤醒的g。
			//
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			//
			// 下面的if只对于初入的g而言，因为唤醒的g的awoke为true。
			//
			// 设置mutexWoken防止持有锁的g在解锁时唤醒队列中的等待者，
			// 持有锁的g看到mutexWoken标志位后不会在尝试唤醒队列中的g。
			// 而是直接返回。
			//
			// 因为这个新到来的g不进入锁队列，而是让其尝试获得锁，能让锁最大效率化。
			//
			// 此时即使允许唤醒锁队列中的g也抢不过这个刚申请锁的g，造成操作浪费。
			// 因为唤醒(加入队列)到调度需要一定的时间。
			//
			// 1. 有等待者，设置 mutexWoken 标志位才会有意义，所以需满足： old>>mutexWaiterShift != 0。
			// 2. 状态要一致性，如果 old 被其它协程改变了，前面的判断基础也就不存在了，需重新获取：所以需满足
			// atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken)==true。
			// 3. 如果已经被唤醒了，解锁者不会再唤醒另外一个g了，所以没有必要设置 mutexWoken 标志位：所以需满足
			// !awoke==true。
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				// 至此: g是新入 Lock 调用的的g。
				//
				// 操作成功之后这个初入G就像一个刚刚唤醒的G一样，因为它成功设置了 mutexWoken 标志位。
				awoke = true
				// 至此：g接下来的行为就像一个刚被唤醒的等待者。
				//
				// 虽然成功设置了 mutexWoken标志位，当接下来的遍历中不一定能获得该锁，
				// 因为还存在其它新入的G
			}
			// 循环调用PAUSE汇编指令30次，不会让出CPU。
			runtime_doSpin()
			iter++
			// 更新原始状态，知道最外层的if条件为false，才能继续后面的逻辑。
			old = m.state
			//持续自旋，直到锁被解除或者自旋次数已超过限制。
			continue
		}
		// 至此：
		// old 需满足以下条件之一或者任意条件的组合：
		// 1. 自选runtime_canSpin函数返回了false（比如已经超过4次循环等）
		// 2. mutexLocked 标志位被清除了。
		// 3. 设置了 mutexStarving 标志位。
		//
		new := old
		// 在old(原始状态)的基础上根据情况设置或取消某些标志位，并将变更后的结果存储到new(目标状态)。
		// 然后调用 atomic.CompareAndSwapInt32(&m.state, old, new)；以便替将原始状态替换为目标
		// 状态。但前提时获取old状态之后到设置之前这段时间old的状态没有变更，一旦变更必须重新获取原始
		// 状态，再根据情况设置新的new。因为new是在old的基础上进行设置了，如果中途old的状态发生了变更
		// ，变更依据便没有了new是一个无效值。
		//
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		//

		if old&mutexStarving == 0 {
			// 没有处于饥饿模式下，还有可能获得锁的，所以在目标状态设置
			// mutexLocked 标志位。
			// 即使old现在已经持有锁了，也没有关系，因为new在old的基础上
			// 只增加了标志位没有重置，所以在这种情况下，下面的 new|=mutexLocked
			// 没有负作用。
			new |= mutexLocked
		}

		if old&(mutexLocked|mutexStarving) != 0 {
			// 如果old已经有锁或者处于饥饿模式，当前g肯定进入等待队列。
			// 所以要增加等地计数。
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		//
		// (这个逻辑是针对被唤醒后的等待者而言才有意义）
		// 因为初入G的starving肯定为false。
		// 如果这个被唤醒的G距离最开始进入休眠的时间间隔超过了1ms且已锁，则设置饥饿模式。
		//
		if starving && old&mutexLocked != 0 {
		// 这两个条件只有正常模式下被唤醒的等待者才有可能满足。
		// 1. starving 成立的条件：正常模式下被唤醒的G且在队列中已经超过了1ms。
		// 2. 唤醒者能获得锁。

		// 由此可见如果想进入饥饿模式：正常模式下被唤醒的G必须没能获得锁情况下才能
		// 将模式设置为饥饿模式。因为要设置 mutexStarving 标志位，old必须设置了
			// mutexLocked 标志位，要想将 mutexStarving 标志存储到 Mutex.state
			// 中 old又必须不能变更。所以这个将正常模式变更到饥饿模式的唤醒者必定会执行
			// runtime_SemacquireMutex(&m.sema, queueLifo, 1)。
			//
			// 不过有可能执行之后，便立即返回。因为解锁着可能会在其调用runtime_SemacquireMutex
			// 之前调用了 runtime_SemreleaseMutex。由于信号量一次release针对一次acquire，不分
			// 先后执行。
			new |= mutexStarving
		}

		if awoke {
			// 至此：
			// g为：1. 新入Lock 调用的g且成功设置了 mutexWoken 标志位。
			//     2. 正常模式下被唤醒的等待者。
			//
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			//
			// 下面3点让下面的if语句肯定不会返回true：
			// 1. 新入 Lock 调用的g，state的mutexWoken标志位肯定设置了。
			//    即使存在新到来的尝试获取锁的g它们也不会改变mutexWoken标志位。
			// 2. 正常模型下被唤醒的g，mutexWoken 被唤醒者所设置。
			// 3. 只有被唤醒的等待者才能清除mutexWoken标志位。因一次只能有一个被唤醒者，
			//    且目前还未执行到清除逻辑。
			//
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
	        // 必须清除唤醒位
			// 因为接下来要么获取锁成功，要么重新进入休眠，不会再继续遍历了。
			new &^= mutexWoken
		}

		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 从开始的获取old，到此刻old状态未发生变更，目标状态顺利设置。
			// 没有获得锁的条件：
			// 1. 原始状态 mutexLocked 标志位已经设置了，这种情况下肯定不能认为获得了锁
			//    因为锁已被其它协程锁持有。
			// 2. 原始状态属于饥饿模式，因为饥饿模式锁的传递有严格的限制，而执行到这里的g
			//    肯定不是一个在饥饿模型被唤醒的g。所以也不能认为获取锁成功。
			//
			// 对于情况2：其实当 old 处于饥饿模式下，new也不会设置 mutexLocked 标志位（见上
			// 面new的设置标志位的条件语句)，所以这种情况下即使old没上锁，new也不会上锁。
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// 至此：目标状态已经存储完成，也没能获得锁。接下来必须进入等待队列。
			//
			// If we were already waiting before, queue at the front of the queue.
			//
			// 正常模式下被唤醒的G，没能获得锁而进入等待队列的会被放在锁队列的队首。
			// 新入 Lock 的g，未能获得锁而进入等待队列的，会放在队列末尾。
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				// 新入 Lock 的g，未能获得锁而进入等待队列的，需更新器 waitStartTime
				// 为入队时间。
				waitStartTime = runtime_nanotime()
			}

			// 进入休眠状态，等待持有锁的g调用unLock唤醒。
			// 至此g可能为：
			// 1. 正常模式下，被唤醒的未获得锁的g。
			// 2. 饥饿模式下，初入的g。
			// 3. 正常模式下，未能获得锁的新入 Lock 调用的g。
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)

			// 被唤醒后，计算总共休眠了多久，因为一个G可以被唤醒多次可能还未获得锁。
			// 所以需要每次别唤醒后都计算一次。
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			// 总等待时间大于1ms，则 starving 为true。
			//
			// 获取最新的原始状态，因为在休眠的过程中状态可能发生了变更。
			old = m.state
			// 只有饥饿模式下被唤醒的g，下面的条件才有可能成立。
			if old&mutexStarving != 0 {
				// 至此：g是一个处于饥饿模式下被唤醒的g。
				//
				// Mutex.state 的状态肯定包括：
				// 1. 未设置 mutexLocked 标志位。
				// 2. 等待计数至少为1。因为在饥饿模式下，解锁者不会为被唤醒的等待者递减引用计数。
				// 3. mutexWoken 标志位肯定没有被设置。
				//
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				//
				// 以下条件都不会成立的原因：
				// mutex处于饥饿模式：
				// 1. 唤醒者G只需消除mutexLocked位然后返回，所以不会设置mutexWoken标志位，同时
				//    也不会存在新入g尝试设置mutexWoken 标志位。
				// 2. 不会有其它初入G来抢锁，所以 mutexLocked 位也不会被设置，交由这个被唤醒者设置。
				// 3. mutexWaiterShift也不可能为0，因为执行到这里的G，还未清除其等待计数。
				//
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}

				// 清除自己的等待计数，并设置mutexLocked状态。
				delta := int32(mutexLocked - 1<<mutexWaiterShift)

				// 满足以下条件之一进入正常模式：
				// 1.g在等待队列中待的总时长不到1ms。
				// 2.g是队列中的最后一个。
				//
				// 以便提高锁的利用率。
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					// 清除饥饿标志位
					delta -= mutexStarving
				}
				// 在 Lock 调用返回时肯定会进行如下动作：
				// 1. 减少等待计数。
				// 2. 设置锁标志位。
				// 3. 如果自己未等待超过1ms或者自己已经是最后一个等待者，则取消饥饿模式标志位。
				atomic.AddInt32(&m.state, delta)
				//获取锁成功，跳出循环，从 Lock 调用返回。
				break
			}
			// 至此：g为正常模式下被唤醒的等待者。
			// 此时 mutex 的状态：
			// 1. 肯定设增了 mutexWoken 标志位。
			// 2. mutexLocked 标志为可能被设置（锁刚释放就被新入 Lock调用的g抢走了)。
			// 3. 等待计数可能为0，因为正常模式下解锁者会替被唤醒者递减等待计数。
			//
			// 设置 awoke 为true，会到循环开始处，要根据 awoke=true来清空由解锁者设置的awoken标志位。
			//
			awoke = true
			// 为了不影响新的自旋，将iter置0,每被唤醒一次会开启一个自旋周期。
			iter = 0
		} else {

			// 至此：
			// 肯定处于正常模式下。
			// g为新入g或者被唤醒g，在执行cas时失败。
			//
			// old状态在获取到切换之间发生变更，以这样old状态
			// 为基础生成的目标状态new是没有意义的。因为假设基础都是错的。
			//
			// 获取新的状态继续遍历。
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock()
// 触发panic的情况：
// 1. Mutex.state 锁标志位未被设置的情况下，调用了此函数。
// 2. 在持有锁的情况下，连续两次调用 Unlock, 中间没有 Lock 调用，其实这是第一种情况的特殊情况。
//
// 一定会做的事情：
// 1. 取消 mutexLocked 标志位。
// 可能会做的事：
// 1. 饥饿模式下唤醒一个等待者，让出当前协程时间片。用来运行等待者，等待者恢复运行时会设置 mutexLocked 标志位。
// 2. 唤醒一个等待者，替其减少等待计数。这个g可能会与新入lock的g发
//    生竞争。不一定能获得锁。
// 唤醒任务交给新锁的持有者。
//
// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	//出现以下情况进入慢解锁模式：
	// 1. 锁队列肯定不为空。
	// 2. 有可能处于饥饿模式。
	// 3. woken标志位可能被设置。
	//
	// 排除其他情况：
	// 1. 队列为空，设置了饥饿模式：
	//    1.1 饥饿模式要么是刚刚被唤醒的g设置的(此时队列为空)，也就是这个g在上一次唤醒时与新入g竞争失败，但再进入休眠前检查到已经等待了1ms，
	//    在还没有入队时，解锁发生了。但是它设置饥饿模式还有一个条件就是队列不为空。所以这种情况不存在。
	//    1.2 已经处于饥饿模式了，也就是这个设置不发生在刚刚解锁之前。因为在饥饿模式下最后一个被唤醒的g会取消饥饿模式。所以解锁的就只能是最后
	//    被唤醒的g了。队列此时是为空，但在其被唤醒时已经取消了饥饿模式。所以在它解锁时肯定不会处于饥饿模式了。
	// 2. 队列为空，设增了woken标志位：
	//    woken标准位可以是解锁方设置也可以获取锁的设置，因为解锁才刚刚开始且解锁方设置的woken标准位在获得锁的g从lock返回时
	//    都会取消woken标志位，所以是尝试获取锁的g设置的。
	//    解锁之前运行的g要么是多个新入的g要么是一个上一轮被唤醒的竞争失败还处于自旋状态的g。它们设置woken标志位的前提是队列中有
	//    等待者。
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	// 至此new表示的状态：肯定未设置locked位，且队列不为空。
	if (new+mutexLocked)&mutexLocked == 0 {
		//1. 防止执行连续两次调用Unlock，而中间没有Lock调用
		//2. 防止未处于锁状态的mutex解锁。
		fatal("sync: unlock of unlocked mutex")
	}

	if new&mutexStarving == 0 {
		// 不处于饥饿模式。
		// 队列肯定不为空。

		old := new
		for {
			// 因为在进入 unlockSlow 之前已经释放了锁，所以下面的
			// 上一个锁的持有者唤醒的G和尝试获取锁的新G都有可能改变 mutex.state
			// 的值。比如：
			// 1. 设置 mutexStarving 标志位(被唤醒的G且等待时间超过1ms锁又被新入G抢占了)
			// 2. 设置 mutexLocked 标志位(获得锁的G)
			// 3. 递增计数 (未能获得锁的新入G)
			// 4. 设置 mutexWoken 标志位（新入G抢占）
			//
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			//
			// 出现以下情况不会唤醒锁等待队列中的G：
			// 1. 锁等待队列为空(可能性大)，不过肯定不是第一轮循环。
			// 2. 锁在刚被释放时便已经被新入(或上次回唤醒的但抢锁失败的g，可能性小)G抢占了。(可能性大)
			// 3. mutexWoken状态被新到来还未进入锁队列的正在进行自旋的G所成功设置(可能性大)。
			//
			//  防止唤醒其它G，提高效率。因为即使唤醒也大概率抢不过那个自旋的G。
			//
			// 分析为何满足以下条件之一便可返回：
			// 1. old>>mutexWaiterShift == 0
			//    说明它释放的锁已经被其它自旋g(新入的或上次唤醒的)，它又唤醒了队列中的g。因为已经不是
			//    锁的持有者了且队列已空可以直接返回了。
			// 2. old 状态包含 mutexLocked，理由同上自己已经不是锁的持有者了，即使唤醒了一个等待者大概
			//    也抢不到锁，因为自己对锁的状态了解程度不如那个新获得锁的g。
			// 3. old 状态包含 mutexWoken，这是一个新入lock的g设置的，目的就是为了防止持有锁的g唤醒等
			//    待者。接下来持有锁的g会负责唤醒等待者(如果有且满足条件的话)
			// 4. mutexStarving，一旦计入了饥饿模式，等待者的唤醒将有严格的限制。即锁的持有者每一次唤醒
			//    一个等待者。所以当前g已经不是锁的持有者了，可以直接返回。
			//
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			//
			// 清除一个等待计数,设置mutexWoken标志位，准备唤醒一个G了。
			// 这个被唤醒的G不一定能成功获得锁。
			// 设置woken标志位后新入G不会再尝试将自己伪装为一个唤醒的G。
			// 但它们依旧存在竞争。
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			// 与饥饿模式下不同，在饥饿模式下唤醒者不必为被唤醒者减少等待计数，它会恢复执行时会自己执行。
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// 唤醒等待中的G
				// G被放入本地P的runnext字段。
				// 但当前G不会让出时间片。
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			//上次循环步获取的状态改变，需重试。
			old = m.state
		}
	} else {
		// mutex目前的状态：
		// 1. mutex当前处于饥饿模式。
		// 2. 且队列不为空。
		// 3. 目前没有处于自旋状态的g。如果解锁前刚刚被唤醒的g设置了饥饿模式位，
		//    即使当时已经存在一些新入的自旋g，也会很快结束。即即使存在也不会改变mutex的状态除
		//    了递增等待计数。
		// 4. woken标志位未被设置。第三步的自旋g可能会设置woken标志位，但是唤醒的g在进入饥饿
		//    模式时会将woken标志位清除并设置饥饿模式位和增加等待计数，它取消了自旋的woken
		//    标志位，自旋锁的旧状态变了会继续迭代但此时已经处于了饥饿模式它也不会再设置woken标志
		//    位了。
		//
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		//
		// 将队列处于队首位置的G，设置到当前P的runnext字段，然后让出时间片。
		// (挂起当前g然后调用schedule()，这样处于runnext字段的G会立即得到执行。)
		// 处于饥饿模式下，这个被直接唤醒的g在退出lock之前会进行如下动作：
		// 1. 减少等待计数。
		// 2. 设置锁标志位。
		// 3. 如果自己未等待超过1ms或者自己已经是最后一个等待者，则取消饥饿模式标志位。
		//
		runtime_Semrelease(&m.sema, true, 1)
	}
}
