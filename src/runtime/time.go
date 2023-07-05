// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Time-related runtime and pieces of package time.

package runtime

import (
	"internal/abi"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// Package time knows the layout of this structure.
// If this struct changes, adjust ../time/sleep.go:/runtimeTimer.
type timer struct {
	// runtime 使用堆结构来管理timer，而每个P中都有一个最小堆。这个PP
	// 字段本质上是个指针，表示当前timer被放置在哪个P的堆中。
	//
	// If this timer is on a heap, which P's heap it is on.
	// puintptr rather than *p to match uintptr in the versions
	// of this struct defined in other packages.
	pp puintptr

	// 时间戳，精确到纳秒级别，表示timer何时回触发。
	//
	// Timer wakes up at when, and then at when+period, ... (period > 0 only)
	// each time calling f(arg, now) in the timer goroutine, so f must be
	// a well-behaved function and not block.
	//
	// when must be positive on an active timer.
	when   int64
	// 表示周期性 timer 的触发周期，单位为纳秒。
	period int64
	// 回调函数，当 timer 触发的时候会调用它，他就是要定时完成的任务。
	f      func(any, uintptr)
	// 调用f时传递给它的第一个参数。
	arg    any
	// 起到一个序号作用，主要被 netpoller 使用。
	seq    uintptr
    // 时间戳，在修改 tiemr 时用来记录修改后的触发时间。修改 timer 时不会直接修改
	// when 字段，这样会打乱堆的有序状态，所以先更新 nextwhen 字段并将 status 设
	// 置为已修改状态，等到后续调整 timer 在堆中位置时再更新 when 字段。
	// What to set the when field to in timerModifiedXX status.
	nextwhen int64

	// 表示 timer 当前的状态，取值自runtime 中一组预定义常量。
	// The status field holds one of the values below.
	status atomic.Uint32
}

// Code outside this file has to be careful in using a timer value.
//
// The pp, status, and nextwhen fields may only be used by code in this file.
//
// Code that creates a new timer value can set the when, period, f,
// arg, and seq fields.
// A new timer value may be passed to addtimer (called by time.startTimer).
// After doing that no fields may be touched.
//
// An active timer (one that has been passed to addtimer) may be
// passed to deltimer (time.stopTimer), after which it is no longer an
// active timer. It is an inactive timer.
// In an inactive timer the period, f, arg, and seq fields may be modified,
// but not the when field.
// It's OK to just drop an inactive timer and let the GC collect it.
// It's not OK to pass an inactive timer to addtimer.
// Only newly allocated timer values may be passed to addtimer.
//
// An active timer may be passed to modtimer. No fields may be touched.
// It remains an active timer.
//
// An inactive timer may be passed to resettimer to turn into an
// active timer with an updated when field.
// It's OK to pass a newly allocated timer value to resettimer.
//
// Timer operations are addtimer, deltimer, modtimer, resettimer,
// cleantimers, adjusttimers, and runtimer.
//
// We don't permit calling addtimer/deltimer/modtimer/resettimer simultaneously,
// but adjusttimers and runtimer can be called at the same time as any of those.
//
// Active timers live in heaps attached to P, in the timers field.
// Inactive timers live there too temporarily, until they are removed.
//
// addtimer:
//   timerNoStatus   -> timerWaiting
//   anything else   -> panic: invalid value
// deltimer:
//   timerWaiting         -> timerModifying -> timerDeleted
//   timerModifiedEarlier -> timerModifying -> timerDeleted
//   timerModifiedLater   -> timerModifying -> timerDeleted
//   timerNoStatus        -> do nothing
//   timerDeleted         -> do nothing
//   timerRemoving        -> do nothing
//   timerRemoved         -> do nothing
//   timerRunning         -> wait until status changes
//   timerMoving          -> wait until status changes
//   timerModifying       -> wait until status changes
// modtimer:
//   timerWaiting    -> timerModifying -> timerModifiedXX
//   timerModifiedXX -> timerModifying -> timerModifiedYY
//   timerNoStatus   -> timerModifying -> timerWaiting
//   timerRemoved    -> timerModifying -> timerWaiting
//   timerDeleted    -> timerModifying -> timerModifiedXX
//   timerRunning    -> wait until status changes
//   timerMoving     -> wait until status changes
//   timerRemoving   -> wait until status changes
//   timerModifying  -> wait until status changes
// cleantimers (looks in P's timer heap):
//   timerDeleted    -> timerRemoving -> timerRemoved
//   timerModifiedXX -> timerMoving -> timerWaiting
// adjusttimers (looks in P's timer heap):
//   timerDeleted    -> timerRemoving -> timerRemoved
//   timerModifiedXX -> timerMoving -> timerWaiting
// runtimer (looks in P's timer heap):
//   timerNoStatus   -> panic: uninitialized timer
//   timerWaiting    -> timerWaiting or
//   timerWaiting    -> timerRunning -> timerNoStatus or
//   timerWaiting    -> timerRunning -> timerWaiting
//   timerModifying  -> wait until status changes
//   timerModifiedXX -> timerMoving -> timerWaiting
//   timerDeleted    -> timerRemoving -> timerRemoved
//   timerRunning    -> panic: concurrent runtimer calls
//   timerRemoved    -> panic: inconsistent timer heap
//   timerRemoving   -> panic: inconsistent timer heap
//   timerMoving     -> panic: inconsistent timer heap

// Values for the timer status field.
const (

	// 一定不再堆中的状态
	//
	// 1. 一个新创建的timer再添加到堆执之前状态，接下来会被
	// 切换至 timerWaiting 状态，然后加锁 执行 addtimer 解锁。
	// 2. 一个正在运行的timer，在运行结束后会先执行 dodeltimer0 从推中删除
	// 然后将其状态由 timerRunning -> timerNoStatus
	//
	// Timer has no status set yet.
	timerNoStatus = iota

	// 表示处于等待状态的 timer，已经被添加到了某个堆中等待出发。
	// Waiting for timer to fire.
	// The timer is in some P's heap.
	timerWaiting
    // 表示运行中的 timer，是一个短暂的状态，也就是触发后 timer.f 执行期间。
	// Running the timer function.
	// A timer will only have this status briefly.
	timerRunning

	// 表示已经删除但还未被移除的 timer，仍位于某个P的堆中，但不会触发。
	// The timer is deleted and should be removed.
	// It should not be run, but it is still in some P's heap.
	timerDeleted

	// 表示正在被从推中移除，也是一个短暂的状态。
	// The timer is being removed.
	// The timer will only have this status briefly.
	timerRemoving

	// 表示已经被从堆中移除了，一般是从 Deleted 到 Removing 最终到 Removed
	// The timer has been stopped.
	// It is not in any P's heap.
	timerRemoved

	// 表示 timer 正在被修改也是一个短暂的状态。
	// The timer is being modified.
	// The timer will only have this status briefly.
	timerModifying

	// 表示 timer 被修改到一个更早的时间。新时间存储在 nextwhen 中
	// The timer has been modified to an earlier time.
	// The new when value is in the nextwhen field.
	// The timer is in some P's heap, possibly in the wrong place.
	timerModifiedEarlier

	// 表示 timer 被修改到一个更晚的时间，新时间存在于 nextwhen 中。
	// The timer has been modified to the same or a later time.
	// The new when value is in the nextwhen field.
	// The timer is in some P's heap, possibly in the wrong place.
	timerModifiedLater

	// 表示一个被修改过的 timer 正在被移动，也是一个短暂的状态。
	// The timer has been modified and is being moved.
	// The timer will only have this status briefly.
	timerMoving
)

// maxWhen is the maximum value for timer's when field.
const maxWhen = 1<<63 - 1

// verifyTimers can be set to true to add debugging checks that the
// timer heaps are valid.
const verifyTimers = false

// Package time APIs.
// Godoc uses the comments in package time, not these.

// time.now is implemented in assembly.

// timeSleep puts the current goroutine to sleep for at least ns nanoseconds.
//
//go:linkname timeSleep time.Sleep
func timeSleep(ns int64) {
	if ns <= 0 {
		return
	}

	gp := getg()
	t := gp.timer
	if t == nil {
		t = new(timer)
		gp.timer = t
	}
	t.f = goroutineReady
	t.arg = gp
	t.nextwhen = nanotime() + ns
	if t.nextwhen < 0 { // check for overflow.
		t.nextwhen = maxWhen
	}
	gopark(resetForSleep, unsafe.Pointer(t), waitReasonSleep, traceEvGoSleep, 1)
}

// resetForSleep is called after the goroutine is parked for timeSleep.
// We can't call resettimer in timeSleep itself because if this is a short
// sleep and there are many goroutines then the P can wind up running the
// timer function, goroutineReady, before the goroutine has been parked.
func resetForSleep(gp *g, ut unsafe.Pointer) bool {
	t := (*timer)(ut)
	resettimer(t, t.nextwhen)
	return true
}

// startTimer()
// 对底层函数 addtimer() 的简单包装，通过 linkname 机制链接到 time 包
// ，提供给标准库使用。这样的函数还有 stopTimer() 和 resetTimer()。
//
// startTimer adds t to the timer heap.
//
//go:linkname startTimer time.startTimer
func startTimer(t *timer) {
	if raceenabled {
		racerelease(unsafe.Pointer(t))
	}
	addtimer(t)
}

// stopTimer()
// 对底层函数 deltimer() 的简单包装，通过 linkname 机制链接到 time 包
// ，提供给标准库使用。这样的函数还有 startTimer() 和 resetTimer()。
// stopTimer stops a timer.
// It reports whether t was stopped before being run.
//
//go:linkname stopTimer time.stopTimer
func stopTimer(t *timer) bool {
	return deltimer(t)
}

// resetTimer()
// 对底层函数 resettimer() 的简单包装，通过 linkname 机制链接到 time 包
// ，提供给标准库使用。这样的函数还有 stopTimer() 和 startTimer()。
//
// resetTimer resets an inactive timer, adding it to the heap.
//
// Reports whether the timer was modified before it was run.
//
//go:linkname resetTimer time.resetTimer
func resetTimer(t *timer, when int64) bool {
	if raceenabled {
		racerelease(unsafe.Pointer(t))
	}
	return resettimer(t, when)
}

// modTimer modifies an existing timer.
//
//go:linkname modTimer time.modTimer
func modTimer(t *timer, when, period int64, f func(any, uintptr), arg any, seq uintptr) {
	modtimer(t, when, period, f, arg, seq)
}

// Go runtime.

// Ready the goroutine arg.
func goroutineReady(arg any, seq uintptr) {
	goready(arg.(*g), 0)
}

// Note: this changes some unsynchronized operations to synchronized operations
// addtimer adds a timer to the current P.
// This should only be called with a newly created timer.
// That avoids the risk of changing the when field of a timer in some P's heap,
// which could cause the heap to become unsorted.
func addtimer(t *timer) {
	// when must be positive. A negative value will cause runtimer to
	// overflow during its delta calculation and never expire other runtime
	// timers. Zero will cause checkTimers to fail to notice the timer.
	if t.when <= 0 {
		throw("timer when must be positive")
	}
	if t.period < 0 {
		throw("timer period must be non-negative")
	}
	if t.status.Load() != timerNoStatus {
		throw("addtimer called with initialized timer")
	}
	// 上面三个if语句是对 when、period和status字段的校验及修正。
	t.status.Store(timerWaiting)

	when := t.when

	// Disable preemption while using pp to avoid changing another P's heap.
	mp := acquirem()

	pp := getg().m.p.ptr()
	// 加锁
	lock(&pp.timersLock)
	// 锁的保护下调用 cleantimers 函数清理堆顶，可能存在已被删除的 timer，再调用
	// doaddtimer() 函数把t添加到推中。
	cleantimers(pp)
	doaddtimer(pp, t)
	// 完成后解锁
	unlock(&pp.timersLock)

	// wakeNetPoller 函数会根据 when 的值按需唤醒阻塞中的 netpoller，目的是让调度
	// 线程及时处理 timer。
	wakeNetPoller(when)

	releasem(mp)
}

// doaddtimer adds t to the current P's heap.
// The caller must have locked the timers for pp.
func doaddtimer(pp *p, t *timer) {
	// Timers rely on the network poller, so make sure the poller
	// has started.
	if netpollInited.Load() == 0 {
		netpollGenericInit()
	}

	if t.pp != 0 {
		throw("doaddtimer: P already set in timer")
	}
	t.pp.set(pp)
	i := len(pp.timers)
	pp.timers = append(pp.timers, t)
	siftupTimer(pp.timers, i)
	if t == pp.timers[0] {
		pp.timer0When.Store(t.when)
	}
	pp.numTimers.Add(1)
}
// deltimer() 主要用来删除一个timer，这里的删除操作主要是修改timer的状态，并不是
// 从推中移除。目的是使其不再被执行。
// 该函数不会改动 timer 堆，所以不需要对 timersLock 加锁。
//
// 返回值：
// 只有这个函数本身将 timer 的状态设置成 timerDeleted 时返回true。
//
// deltimer deletes the timer t. It may be on some other P, so we can't
// actually remove it from the timers heap. We can only mark it as deleted.
// It will be removed in due course by the P whose heap it is on.
// Reports whether the timer was removed before it was run.
func deltimer(t *timer) bool {
	for {
		switch s := t.status.Load(); s {
		case timerWaiting, timerModifiedLater:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			mp := acquirem()
			// 先切换到 timerModifying 一种保护状态
			if t.status.CompareAndSwap(s, timerModifying) {
				// Must fetch t.pp before changing status,
				// as cleantimers in another goroutine
				// can clear t.pp of a timerDeleted timer.
				tpp := t.pp.ptr()
				if !t.status.CompareAndSwap(timerModifying, timerDeleted) {
					// 一致性校验， 处于 timerModifying 状态的 timer 不应该被其它G所修改
					badTimer()
				}
				releasem(mp)
				tpp.deletedTimers.Add(1)
				// Timer was not yet run.
				return true
			} else {
				// 切换失败说明另外一个线程也在修改这个timer
				// 继续对其它状态进行操作
				releasem(mp)
			}
		case timerModifiedEarlier:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			mp := acquirem()
			if t.status.CompareAndSwap(s, timerModifying) {
				// Must fetch t.pp before setting status
				// to timerDeleted.
				tpp := t.pp.ptr()
				if !t.status.CompareAndSwap(timerModifying, timerDeleted) {
					badTimer()
				}
				releasem(mp)
				tpp.deletedTimers.Add(1)
				// Timer was not yet run.
				return true
			} else {
				releasem(mp)
			}
		case timerDeleted, timerRemoving, timerRemoved:
			// 要么不会再触发(目的已经达到)、要不更本不在堆中
			//
			// Timer was already run.
			return false
		case timerRunning, timerMoving:
			// 一种短暂的过度状态，yield暂时让出线程时间片。
			//
			// The timer is being run or moved, by a different P.
			// Wait for it to complete.
			osyield()
		case timerNoStatus:
			// 不在堆中
			//
			// Removing timer that was never added or
			// has already been run. Also see issue 21874.
			return false
		case timerModifying:
			// 短暂过度状态继续等待
			//
			// Simultaneous calls to deltimer and modtimer.
			// Wait for the other call to complete.
			osyield()
		default:
			badTimer()
		}
	}
}
// dodeltimer()
// tiemrs[i]=tierm[last];tiemr[last]=nil;tiemrs=[:last]
// 这样就移除了处于i位置的timer。
// 同时此函数还会将新的 timer[i] 移动到正确的位置。
//
// 返回值：
// 将制定的timer从堆中移除，只被runtime中的timer模块使用
//
// dodeltimer removes timer i from the current P's heap.
// We are locked on the P when this is called.
// It returns the smallest changed index in pp.timers.
// The caller must have locked the timers for pp.
func dodeltimer(pp *p, i int) int {
	if t := pp.timers[i]; t.pp.ptr() != pp {
		throw("dodeltimer: wrong P")
	} else {
		t.pp = 0
	}
	last := len(pp.timers) - 1
	if i != last {
		pp.timers[i] = pp.timers[last]
	}
	pp.timers[last] = nil
	pp.timers = pp.timers[:last]
	smallestChanged := i
	if i != last {
		// Moving to i may have moved the last timer to a new parent,
		// so sift up to preserve the heap guarantee.
		smallestChanged = siftupTimer(pp.timers, i)
		siftdownTimer(pp.timers, i)
	}
	if i == 0 {
		updateTimer0When(pp)
	}
	n := pp.numTimers.Add(-1)
	if n == 0 {
		// If there are no timers, then clearly none are modified.
		pp.timerModifiedEarliest.Store(0)
	}
	return smallestChanged
}

// dodeltimer0 removes timer 0 from the current P's heap.
// We are locked on the P when this is called.
// It reports whether it saw no problems due to races.
// The caller must have locked the timers for pp.
func dodeltimer0(pp *p) {
	if t := pp.timers[0]; t.pp.ptr() != pp {
		throw("dodeltimer0: wrong P")
	} else {
		t.pp = 0
	}
	last := len(pp.timers) - 1
	if last > 0 {
		pp.timers[0] = pp.timers[last]
	}
	pp.timers[last] = nil
	pp.timers = pp.timers[:last]
	if last > 0 {
		siftdownTimer(pp.timers, 0)
	}
	updateTimer0When(pp)
	n := pp.numTimers.Add(-1)
	if n == 0 {
		// If there are no timers, then clearly none are modified.
		pp.timerModifiedEarliest.Store(0)
	}
}
// 函数执行分为两个部分：
// 1. 凡是以ing结尾的状态都表示需要等待知道一种确定状态除了 timerWaiting。
// 2. 无需等待的状态：
//   2.1 timerNoStatus 、 timerRemoved 表示t已经不在堆中了，需要 -> timerModifying (添加到堆中) ->
//       timerWaiting
//   2.2 timerDeleted 、 timerModifiedEarlier 、 tiemrModifiedLater 和 timerWaiting 状态的t表示
//       在堆中，只需变更状态。根据 when 和 t.when 进行比较的结果转移到 -> timerModifiedEarlier 或
//       timerModifiedLater。
// 对于原本就在堆中的timer，需要把新的时间（参数when）赋值给它的 nextwhen 字段，而不是直接改动它的 when 字段
// ，因为在这里不打算改动它在堆中的位置。
// 1. 如果最终状态为 timerModifiedEarlier 则会尝试修改关联P的timerModifiedEarliest字段。
// 2. 如果timer是添加到堆中或者变更到 timerModifiedEarlier 都会尝试唤醒 netpoll
//
// 返回值：
// 状态由 timerWaiting、 timerModifiedEarlier 和 timerModifiedLater 变更会返回true。
//
// 参数：
// when timer新的运行时间
// period timer.period 的新值
// f timer.f 的新值
// arg timer.arg 的新值
// seq timer.seq 的新值
//
// runtime timer 模块提供的接口供其它runtime中的模块使用，如 netpoll 模块。
//
// modtimer modifies an existing timer.
// This is called by the netpoll code or time.Ticker.Reset or time.Timer.Reset.
// Reports whether the timer was modified before it was run.
func modtimer(t *timer, when, period int64, f func(any, uintptr), arg any, seq uintptr) bool {
	if when <= 0 {
		throw("timer when must be positive")
	}
	if period < 0 {
		throw("timer period must be non-negative")
	}

	status := uint32(timerNoStatus)
	wasRemoved := false
	var pending bool
	var mp *m
loop:
	for {
		// ing结尾的状态除了 timerWaiting 都先让出线程时间片然后再继续循环等待其进入一种非ing状态
		// 对于非ing状态将其变更到 tiemrModifying 状态。
		// 如果 timer 还没有运行或者停止的话， pending设置为true否则为false。
		// 如果 timer 由 timerNoStatus 或 tiemrRemoved 状态变更到 timerModifying 状态，wasRemoved 设为true。
		// 表示循环之后的逻辑为timer添加逻辑而不是修改timer。

		switch status = t.status.Load(); status {
		case timerWaiting, timerModifiedEarlier, timerModifiedLater:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			mp = acquirem()
			if t.status.CompareAndSwap(status, timerModifying) {
				pending = true // timer not yet run
				break loop
			}
			releasem(mp)
		case timerNoStatus, timerRemoved:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			mp = acquirem()

			// Timer was already run and t is no longer in a heap.
			// Act like addtimer.
			if t.status.CompareAndSwap(status, timerModifying) {
				wasRemoved = true
				pending = false // timer already run or stopped
				break loop
			}
			releasem(mp)
		case timerDeleted:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			mp = acquirem()
			if t.status.CompareAndSwap(status, timerModifying) {
				t.pp.ptr().deletedTimers.Add(-1)
				pending = false // timer already stopped
				break loop
			}
			releasem(mp)
		case timerRunning, timerRemoving, timerMoving:
			// The timer is being run or moved, by a different P.
			// Wait for it to complete.
			osyield()
		case timerModifying:
			// Multiple simultaneous calls to modtimer.
			// Wait for the other call to complete.
			osyield()
		default:
			badTimer()
		}
	}
	// 至此 timer 的状态为 timerModifying, 变量 wasRemoved 和 pending 都已经确定。

	// 此时timer已经是 timerModifying 状态了，所以可以安全更新以下字段。
	t.period = period
	t.f = f
	t.arg = arg
	t.seq = seq

	if wasRemoved {
		// timer添加逻辑
		t.when = when
		pp := getg().m.p.ptr()
		lock(&pp.timersLock)
		doaddtimer(pp, t)
		unlock(&pp.timersLock)
		if !t.status.CompareAndSwap(timerModifying, timerWaiting) {
			badTimer()
		}
		releasem(mp)
		// 有新的timer添加了，有新的机会唤醒 netpoll
		wakeNetPoller(when)
	} else {
		// The timer is in some other P's heap, so we can't change
		// the when field. If we did, the other P's heap would
		// be out of order. So we put the new when value in the
		// nextwhen field, and let the other P set the when field
		// when it is prepared to resort the heap.
		//
		// timer 有可能在其他P的堆中，所以我们不直接修改when字段。
		//
		t.nextwhen = when

		newStatus := uint32(timerModifiedLater)
		if when < t.when {
			newStatus = timerModifiedEarlier
		}

		tpp := t.pp.ptr()

		if newStatus == timerModifiedEarlier {
			// 因为变更到了timerModifiedEarlier状态，所以要尝试更新p的timerModifiedEarliest字段
			updateTimerModifiedEarliest(tpp, when)
		}

		// Set the new status of the timer.
		if !t.status.CompareAndSwap(timerModifying, newStatus) {
			badTimer()
		}
		releasem(mp)

		// If the new status is earlier, wake up the poller.
		if newStatus == timerModifiedEarlier {
			// 尝试唤醒 netPoll ，以便及时运行到期的他timer。
			wakeNetPoller(when)
		}
	}

	return pending
}

// resettimer resets the time when a timer should fire.
// If used for an inactive timer, the timer will become active.
// This should be called instead of addtimer if the timer value has been,
// or may have been, used previously.
// Reports whether the timer was modified before it was run.
func resettimer(t *timer, when int64) bool {
	return modtimer(t, when, t.period, t.f, t.arg, t.seq)
}

// cleantimers cleans up the head of the timer queue. This speeds up
// programs that create and delete timers; leaving them in the heap
// slows down addtimer. Reports whether no timer problems were found.
// The caller must have locked the timers for pp.
func cleantimers(pp *p) {
	gp := getg()
	for {
		if len(pp.timers) == 0 {
			return
		}

		// This loop can theoretically run for a while, and because
		// it is holding timersLock it cannot be preempted.
		// If someone is trying to preempt us, just return.
		// We can clean the timers later.
		if gp.preemptStop {
			return
		}

		t := pp.timers[0]
		if t.pp.ptr() != pp {
			throw("cleantimers: bad p")
		}
		switch s := t.status.Load(); s {
		case timerDeleted:
			if !t.status.CompareAndSwap(s, timerRemoving) {
				continue
			}
			dodeltimer0(pp)
			if !t.status.CompareAndSwap(timerRemoving, timerRemoved) {
				badTimer()
			}
			pp.deletedTimers.Add(-1)
		case timerModifiedEarlier, timerModifiedLater:
			if !t.status.CompareAndSwap(s, timerMoving) {
				continue
			}
			// Now we can change the when field.
			t.when = t.nextwhen
			// Move t to the right position.
			dodeltimer0(pp)
			doaddtimer(pp, t)
			if !t.status.CompareAndSwap(timerMoving, timerWaiting) {
				badTimer()
			}
		default:
			// Head of timers does not need adjustment.
			return
		}
	}
}

// moveTimers moves a slice of timers to pp. The slice has been taken
// from a different P.
// This is currently called when the world is stopped, but the caller
// is expected to have locked the timers for pp.
func moveTimers(pp *p, timers []*timer) {
	for _, t := range timers {
	loop:
		for {
			switch s := t.status.Load(); s {
			case timerWaiting:
				if !t.status.CompareAndSwap(s, timerMoving) {
					continue
				}
				t.pp = 0
				doaddtimer(pp, t)
				if !t.status.CompareAndSwap(timerMoving, timerWaiting) {
					badTimer()
				}
				break loop
			case timerModifiedEarlier, timerModifiedLater:
				if !t.status.CompareAndSwap(s, timerMoving) {
					continue
				}
				t.when = t.nextwhen
				t.pp = 0
				doaddtimer(pp, t)
				if !t.status.CompareAndSwap(timerMoving, timerWaiting) {
					badTimer()
				}
				break loop
			case timerDeleted:
				if !t.status.CompareAndSwap(s, timerRemoved) {
					continue
				}
				t.pp = 0
				// We no longer need this timer in the heap.
				break loop
			case timerModifying:
				// Loop until the modification is complete.
				osyield()
			case timerNoStatus, timerRemoved:
				// We should not see these status values in a timers heap.
				badTimer()
			case timerRunning, timerRemoving, timerMoving:
				// Some other P thinks it owns this timer,
				// which should not happen.
				badTimer()
			default:
				badTimer()
			}
		}
	}
}

// adjusttimers
// 目的：
// 1. 移除处于删除状态的tiemr： timerDeleted -> timerRemoving (由 dodeltimer 函数执行具体的移除任务）-> timerRemoved
// 2. 将 timerModifiedLater 和 timerModifiedEarlier 状态改为 timerMoving ，并添加到 moved队列，然后摘一同添加
// ( addAdjustedTimers )-> timerWaiting。
// 3. timerModifying 状态的 timer 继续遍历等待其到一个确定状态。
//
// 运行条件：参数now的值小于 pp.timerModifiedEarliest 或者 pp.timerModifiedEarliest 的值为0
// 结果：这个函数调用之后pp的tiemrs堆中没有处于modifier状态的timer且都是按照它们的when字段进行有序排列。
//      timerModifiedEarliest字段也被设置成了0。
//
// 被调度器调用，除了这个函数还有 runtimer() 和 clearDeletedTimers() 函数，它们对 timer
// 堆进行维护，以及运行那些到达触发时间的 timer。
//
// adjusttimers looks through the timers in the current P's heap for
// any timers that have been modified to run earlier, and puts them in
// the correct place in the heap. While looking for those timers,
// it also moves timers that have been modified to run later,
// and removes deleted timers. The caller must have locked the timers for pp.
func adjusttimers(pp *p, now int64) {
	// If we haven't yet reached the time of the first timerModifiedEarlier
	// timer, don't do anything. This speeds up programs that adjust
	// a lot of timers back and forth if the timers rarely expire.
	// We'll postpone looking through all the adjusted timers until
	// one would actually expire.
	first := pp.timerModifiedEarliest.Load()
	// 如果没有 timerModifiedEarLier 状态的 timer或者最到到期的且处于timerModifiedEarlier
	// 状态的 timer 小于参数 now 则直接返回。因为调整堆的目的就是为了接下来执行 runtimer
	if first == 0 || first > now {
		if verifyTimers {
			verifyTimerHeap(pp)
		}
		return
	}

	// We are going to clear all timerModifiedEarlier timers.
	//
	// 本函数返回后，pp.timers 中不会在有处于 timerModifiedEarlier 或者 timerModifiedLater
	// 状态的timer。
	pp.timerModifiedEarliest.Store(0)

	var moved []*timer
	for i := 0; i < len(pp.timers); i++ {
		t := pp.timers[i]
		if t.pp.ptr() != pp {
			throw("adjusttimers: bad p")
		}
		switch s := t.status.Load(); s {
		case timerDeleted:
			if t.status.CompareAndSwap(s, timerRemoving) {
				changed := dodeltimer(pp, i)
				if !t.status.CompareAndSwap(timerRemoving, timerRemoved) {
					badTimer()
				}
				pp.deletedTimers.Add(-1)
				// Go back to the earliest changed heap entry.
				// "- 1" because the loop will add 1.
				//
				// 因为 dodeltimer 对堆进行了调整，导致一些可能还没被迭代的 timer 移动到了之前的位置
				// ,而chaned的值为变更位置的那些timers的新位置索引中最小的那个索引值。
				// -1抵消循环末尾的 i++，虽然这样可能会把遍历过的timer再遍历一次但不会漏掉。
				i = changed - 1
			}
		case timerModifiedEarlier, timerModifiedLater:
			// 这两种状态的timer先移除再统一添加，这样主循环只用遍历一次。
			if t.status.CompareAndSwap(s, timerMoving) {
				// Now we can change the when field.
				t.when = t.nextwhen
				// Take t off the heap, and hold onto it.
				// We don't add it back yet because the
				// heap manipulation could cause our
				// loop to skip some other timer.
				changed := dodeltimer(pp, i)
				moved = append(moved, t)
				// Go back to the earliest changed heap entry.
				// "- 1" because the loop will add 1.
				//
				// 作用同上
				i = changed - 1
			}
		case timerNoStatus, timerRunning, timerRemoving, timerRemoved, timerMoving:
			// timerNoStatus addtimer() 函数调用中会将timerNoStatus状态的timer在锁的保护下添加到堆中并变更到timerWaiting状态，然后再释放锁。
			// timerRunning  runtimer -> runOneTimer timerWaiting->timerRunning->timerDeleted/ 也是在锁的保护下进行的，然后再释放锁。
			//
			// timerRemoving timerRemoved：timerDeleted -> timerRemoving -> dodeltimer() -> timerRemoved；adjusttimers、cleantimers
			// 和clearDeletedTimers函数中会发生这些变更但也都是在有锁状态下完成的。
			//
			// timerMoving：addjusttimers、addAdjustedTimers 、 clerntimers 、clearDeletedTiemrs 、 moverTimers 和 runtimer 函数都会将
			// 某一状态变更到 timerMoving 然后变更到 timerWaiting。它们也都是在有锁的情况下变更这些状态的。
			//
			// 这些状态的 timer 不会出现这个有锁状态的遍历中。
			badTimer()
		case timerWaiting:
			// OK, nothing to do.
		case timerModifying:
			// Check again after modification is complete.
			osyield()
			// 抵消 i++，因为要继续处理这个tiemr。
			i--
		default:
			badTimer()
		}
	}

	if len(moved) > 0 {
		// 批量插入，这样上面的循环只用遍历一次即可。
		addAdjustedTimers(pp, moved)
	}

	if verifyTimers {
		// 堆正确性校验
		verifyTimerHeap(pp)
	}
}

// addAdjustedTimers adds any timers we adjusted in adjusttimers
// back to the timer heap.
func addAdjustedTimers(pp *p, moved []*timer) {
	for _, t := range moved {
		doaddtimer(pp, t)
		if !t.status.CompareAndSwap(timerMoving, timerWaiting) {
			badTimer()
		}
	}
}

// nobarrierWakeTime looks at P's timers and returns the time when we
// should wake up the netpoller. It returns 0 if there are no timers.
// This function is invoked when dropping a P, and must run without
// any write barriers.
//
//go:nowritebarrierrec
func nobarrierWakeTime(pp *p) int64 {
	next := pp.timer0When.Load()
	nextAdj := pp.timerModifiedEarliest.Load()
	if next == 0 || (nextAdj != 0 && nextAdj < next) {
		next = nextAdj
	}
	return next
}
// runtimer()
// 被调度器调用，除了这个函数还有 adjusttimers() 和 clearDeletedTimers() 函数，它们对 timer
// 堆进行维护，以及运行那些到达触发时间的 timer。
//
// 返回值：1. 大于0，pp的timers堆中按照字段when排序，最小的when值但又小于now，这个值可以用于指导netpoll阻塞时间
//        2. 等于0，成功运行了一个timer，返回0。
//        3. -1  ， 推中没有timer了。
// 必须在系统栈上调用
//
// runtimer examines the first timer in timers. If it is ready based on now,
// it runs the timer and removes or updates it.
// Returns 0 if it ran a timer, -1 if there are no more timers, or the time
// when the first timer should run.
// The caller must have locked the timers for pp.
// If a timer is run, this will temporarily unlock the timers.(防止timer.f函数中调用了
// 一些又尝试获取锁的函数)
//
//go:systemstack
func runtimer(pp *p, now int64) int64 {
	for {
		// 总是从堆顶开始检查
		t := pp.timers[0]
		if t.pp.ptr() != pp {
			throw("runtimer: bad p")
		}
		switch s := t.status.Load(); s {
		case timerWaiting:
			// 这个函数之前一般已经调用过了 adjusttiemrs()函数，所以堆中的timer
			// 的tiemr.when就是下一次运行时间。
			if t.when > now {
				// Not ready to run.
				return t.when
			}

			if !t.status.CompareAndSwap(s, timerRunning) {
				// 其它G可能在进行并更状态操作比如对此timer调用了 deltetiemr()函数
				// ，它不需要锁的状态下便可以变更timer的状态。
				continue
			}
			// Note that runOneTimer may temporarily unlock
			// pp.timersLock.
			// 交付给 runOneTimer 函数的是一个处于 timerRunning状态的timer。
			runOneTimer(pp, t, now)
			return 0

		case timerDeleted:
			// 移除timer
			if !t.status.CompareAndSwap(s, timerRemoving) {
				// modtimer 函数可能会在没有锁的状态下对 timerDeleted 状态进行变更
				continue
			}
			dodeltimer0(pp)
			if !t.status.CompareAndSwap(timerRemoving, timerRemoved) {
				badTimer()
			}
			pp.deletedTimers.Add(-1)
			if len(pp.timers) == 0 {
				return -1
			}

		case timerModifiedEarlier, timerModifiedLater:
			// 先移除再添加
			// tiemrModified.. -> timerMoving -> tiemrWaiting
			if !t.status.CompareAndSwap(s, timerMoving) {
				continue
			}
			t.when = t.nextwhen
			dodeltimer0(pp)
			doaddtimer(pp, t)
			if !t.status.CompareAndSwap(timerMoving, timerWaiting) {
				badTimer()
			}

		case timerModifying:
			// modtiemr 函有能力在不持有锁的情况下，修改timer到此状态。
			// Wait for modification to complete.
			osyield()

		case timerNoStatus, timerRemoved:
			// 在此函数持有锁之后，timers中的tiemr不会出现这两种状态，因为如果堆中的timer
			// 需要变更到这两种状态必须持有锁。
			//
			// Should not see a new or inactive timer on the heap.
			badTimer()
		case timerRunning, timerRemoving, timerMoving:
			// These should only be set when timers are locked,
			// and we didn't do it.
			badTimer()
		default:
			badTimer()
		}
	}
}

// runOneTimer() 目前只被 runtimer() 函数所调用。
// 目的：运行参数t的f字段表示的函数。
// 参数: t的状态肯定是 timerRunning，且其 when字段小于等于参数now。
// 结果：timer要么从参数pp的tiemrs堆中删除要么修改timer.next
//
// runOneTimer runs a single timer.
// The caller must have locked the timers for pp.
// This will temporarily unlock the timers while running the timer function.
//
//go:systemstack
func runOneTimer(pp *p, t *timer, now int64) {
	if raceenabled {
		ppcur := getg().m.p.ptr()
		if ppcur.timerRaceCtx == 0 {
			ppcur.timerRaceCtx = racegostart(abi.FuncPCABIInternal(runtimer) + sys.PCQuantum)
		}
		raceacquirectx(ppcur.timerRaceCtx, unsafe.Pointer(t))
	}

	f := t.f
	arg := t.arg
	seq := t.seq

	if t.period > 0 {
		// 说明 t还需留在堆中
		// Leave in heap but adjust next time to fire.
		delta := t.when - now
		t.when += t.period * (1 + -delta/t.period)
		// t.when 保存下一次运行的时间
		if t.when < 0 { // check for overflow.
			// 正常情况下不会发生
			t.when = maxWhen
		}
		// 修改了when字段堆可能已经不是有序的了需要调整。
		siftdownTimer(pp.timers, 0)
		if !t.status.CompareAndSwap(timerRunning, timerWaiting) {
			badTimer()
		}
		updateTimer0When(pp)
	} else {
		// Remove from heap.
		dodeltimer0(pp)
		if !t.status.CompareAndSwap(timerRunning, timerNoStatus) {
			badTimer()
		}
	}

	if raceenabled {
		// Temporarily use the current P's racectx for g0.
		gp := getg()
		if gp.racectx != 0 {
			throw("runOneTimer: unexpected racectx")
		}
		gp.racectx = gp.m.p.ptr().timerRaceCtx
	}

	unlock(&pp.timersLock)

	// 防止f函数中又调用了需要上pp.tiemrsLock 的函数，造成死锁。
	f(arg, seq)

	lock(&pp.timersLock)

	if raceenabled {
		gp := getg()
		gp.racectx = 0
	}
}
// clearDeletedTimers()
// 被调度器调用，除了这个函数还有 runtimer() 和 runtimer() 函数，它们对 timer
// 堆进行维护，以及运行那些到达触发时间的 timer。
//
// clearDeletedTimers removes all deleted timers from the P's timer heap.
// This is used to avoid clogging up the heap if the program
// starts a lot of long-running timers and then stops them.
// For example, this can happen via context.WithTimeout.
//
// This is the only function that walks through the entire timer heap,
// other than moveTimers which only runs when the world is stopped.
//
// The caller must have locked the timers for pp.
func clearDeletedTimers(pp *p) {
	// We are going to clear all timerModifiedEarlier timers.
	// Do this now in case new ones show up while we are looping.
	pp.timerModifiedEarliest.Store(0)

	cdel := int32(0)
	to := 0
	changedHeap := false
	timers := pp.timers
nextTimer:
	for _, t := range timers {
		for {
			switch s := t.status.Load(); s {
			case timerWaiting:
				if changedHeap {
					timers[to] = t
					siftupTimer(timers, to)
				}
				to++
				continue nextTimer
			case timerModifiedEarlier, timerModifiedLater:
				if t.status.CompareAndSwap(s, timerMoving) {
					t.when = t.nextwhen
					timers[to] = t
					siftupTimer(timers, to)
					to++
					changedHeap = true
					if !t.status.CompareAndSwap(timerMoving, timerWaiting) {
						badTimer()
					}
					continue nextTimer
				}
			case timerDeleted:
				if t.status.CompareAndSwap(s, timerRemoving) {
					t.pp = 0
					cdel++
					if !t.status.CompareAndSwap(timerRemoving, timerRemoved) {
						badTimer()
					}
					changedHeap = true
					continue nextTimer
				}
			case timerModifying:
				// Loop until modification complete.
				osyield()
			case timerNoStatus, timerRemoved:
				// We should not see these status values in a timer heap.
				badTimer()
			case timerRunning, timerRemoving, timerMoving:
				// Some other P thinks it owns this timer,
				// which should not happen.
				badTimer()
			default:
				badTimer()
			}
		}
	}

	// Set remaining slots in timers slice to nil,
	// so that the timer values can be garbage collected.
	for i := to; i < len(timers); i++ {
		timers[i] = nil
	}

	pp.deletedTimers.Add(-cdel)
	pp.numTimers.Add(-cdel)

	timers = timers[:to]
	pp.timers = timers
	updateTimer0When(pp)

	if verifyTimers {
		verifyTimerHeap(pp)
	}
}

// verifyTimerHeap verifies that the timer heap is in a valid state.
// This is only for debugging, and is only called if verifyTimers is true.
// The caller must have locked the timers.
func verifyTimerHeap(pp *p) {
	for i, t := range pp.timers {
		if i == 0 {
			// First timer has no parent.
			continue
		}

		// The heap is 4-ary. See siftupTimer and siftdownTimer.
		p := (i - 1) / 4
		if t.when < pp.timers[p].when {
			print("bad timer heap at ", i, ": ", p, ": ", pp.timers[p].when, ", ", i, ": ", t.when, "\n")
			throw("bad timer heap")
		}
	}
	if numTimers := int(pp.numTimers.Load()); len(pp.timers) != numTimers {
		println("timer heap len", len(pp.timers), "!= numTimers", numTimers)
		throw("bad timer heap len")
	}
}

// updateTimer0When sets the P's timer0When field.
// The caller must have locked the timers for pp.
func updateTimer0When(pp *p) {
	if len(pp.timers) == 0 {
		pp.timer0When.Store(0)
	} else {
		pp.timer0When.Store(pp.timers[0].when)
	}
}

// updateTimerModifiedEarliest updates the recorded nextwhen field of the
// earlier timerModifiedEarier value.
// The timers for pp will not be locked.
func updateTimerModifiedEarliest(pp *p, nextwhen int64) {
	for {
		old := pp.timerModifiedEarliest.Load()
		if old != 0 && int64(old) < nextwhen {
			return
		}

		if pp.timerModifiedEarliest.CompareAndSwap(old, nextwhen) {
			return
		}
	}
}

// timeSleepUntil returns the time when the next timer should fire. Returns
// maxWhen if there are no timers.
// This is only called by sysmon and checkdead.
func timeSleepUntil() int64 {
	next := int64(maxWhen)

	// Prevent allp slice changes. This is like retake.
	lock(&allpLock)
	for _, pp := range allp {
		if pp == nil {
			// This can happen if procresize has grown
			// allp but not yet created new Ps.
			continue
		}

		w := pp.timer0When.Load()
		if w != 0 && w < next {
			next = w
		}

		w = pp.timerModifiedEarliest.Load()
		if w != 0 && w < next {
			next = w
		}
	}
	unlock(&allpLock)

	return next
}

// Heap maintenance algorithms.
// These algorithms check for slice index errors manually.
// Slice index error can happen if the program is using racy
// access to timers. We don't want to panic here, because
// it will cause the program to crash with a mysterious
// "panic holding locks" message. Instead, we panic while not
// holding a lock.

// siftupTimer puts the timer at position i in the right place
// in the heap by moving it up toward the top of the heap.
// It returns the smallest changed index.
func siftupTimer(t []*timer, i int) int {
	if i >= len(t) {
		badTimer()
	}
	when := t[i].when
	if when <= 0 {
		badTimer()
	}
	tmp := t[i]
	for i > 0 {
		p := (i - 1) / 4 // parent
		if when >= t[p].when {
			break
		}
		t[i] = t[p]
		i = p
	}
	if tmp != t[i] {
		t[i] = tmp
	}
	return i
}

// siftdownTimer puts the timer at position i in the right place
// in the heap by moving it down toward the bottom of the heap.
func siftdownTimer(t []*timer, i int) {
	n := len(t)
	if i >= n {
		badTimer()
	}
	when := t[i].when
	if when <= 0 {
		badTimer()
	}
	tmp := t[i]
	for {
		c := i*4 + 1 // left child
		c3 := c + 2  // mid child
		if c >= n {
			break
		}
		w := t[c].when
		if c+1 < n && t[c+1].when < w {
			w = t[c+1].when
			c++
		}
		if c3 < n {
			w3 := t[c3].when
			if c3+1 < n && t[c3+1].when < w3 {
				w3 = t[c3+1].when
				c3++
			}
			if w3 < w {
				w = w3
				c = c3
			}
		}
		if w >= when {
			break
		}
		t[i] = t[c]
		i = c
	}
	if tmp != t[i] {
		t[i] = tmp
	}
}

// badTimer is called if the timer data structures have been corrupted,
// presumably due to racy use by the program. We panic here rather than
// panicing due to invalid slice access while holding locks.
// See issue #25686.
func badTimer() {
	throw("timer data corruption")
}
