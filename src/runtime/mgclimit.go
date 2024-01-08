// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "runtime/internal/atomic"

// gcCPULimiter is a mechanism to limit GC CPU utilization in situations
// where it might become excessive and inhibit application progress (e.g.
// a death spiral).
//
// The core of the limiter is a leaky bucket mechanism that fills with GC
// CPU time and drains with mutator time. Because the bucket fills and
// drains with time directly (i.e. without any weighting), this effectively
// sets a very conservative limit of 50%. This limit could be enforced directly,
// however, but the purpose of the bucket is to accommodate spikes in GC CPU
// utilization without hurting throughput.
//
// Note that the bucket in the leaky bucket mechanism can never go negative,
// so the GC never gets credit for a lot of CPU time spent without the GC
// running. This is intentional, as an application that stays idle for, say,
// an entire day, could build up enough credit to fail to prevent a death
// spiral the following day. The bucket's capacity is the GC's only leeway.
//
// The capacity thus also sets the window the limiter considers. For example,
// if the capacity of the bucket is 1 cpu-second, then the limiter will not
// kick in until at least 1 full cpu-second in the last 2 cpu-second window
// is spent on GC CPU time.
//
// 只会统计：
// 1. 执行GC任务花费的时间，由执行gc的g自己负责记录，记录到其执行时所在的p的对应字段上。
// 1.1 辅助GC的时间/idle GC的时间。dedicated GC time不会记录（通过total*gcBackgroundUtilization 计算而来）
// 2. p停靠在idlep list起和从idlep list出来的这段时间，由负值入队的g和出队的g更新到这个p的对应字段上。
//
// 统计的时间:
// 1. 要记录某类型的事件的执行时间时，会通过调用 p.limiterEvent.start 开始对此事件的记录。
// 2. 当此类型事件结束时，通过调用p.limiterEvent.stop，用结束时间与开启时间的差值作为此类型事件的持续时间。
//    增加到gcCPULimiter对应事件类型的的全局冲池。
//
// 读取时间(一轮update):
// * 执行时机
// ** 1. needUpdate函数判断距离上次执行update是否超过10ms。在findRunnableGCWorker中进行判断。
// ** 2. pp.gcAssistTime > gcAssistTimeSlack(5000纳秒) ，时会调用 update。辅助GC执行时间会累加到pp.gcAssistTime上。
// ** 当超过gcAssistTimeSlack时，先执行update，然后将pp.gcAssistTime记录到gcController.assistTime上之后置零。
//
// 将gcCPULimiter的所有事件缓存时里的时间取出，并将所有时间缓存池置零。然后再将通过调用 p.kimiterEvent.consume将
// 所有p中还没有停止记录的事件的已执行时间取出。
//
// 将当前时间与上次执行update方法的时间的差值作为 windowTime。totalWindowTime=GOAMXPROCS*windowTime。
//
// 总CPU时间为：total=(now-lastUpdate)*GOMAXPROCS
// GC时间：total*gcBackgroundUtilization + assistTimePool 里的时间
// 非GC时间：total - idleTimePool - GC时间。
//
// 将当前时间存储到 lastUpdate 上，为下一轮update做准备。
// 通过这些GC时间和非GC时间来更新桶的状态，根据桶的状态来决定limiter是否开启。
//
// 从要统计的信息量来看，此limiter实现对程序没有什么负担。
var gcCPULimiter gcCPULimiterState

type gcCPULimiterState struct {
	lock atomic.Uint32

	// limiter 当前状态，为true表示开启。协程在分配内存时可以不顾gcAssibytes(但仍然会减少)而
	// 直接分配内存。
	enabled atomic.Bool
	// bucket 是一个泄露桶，不会出现负债和盈余。
	//
	// 需从bucket.fill中减去非GC时间，但需满足fill最小为0。
	// 需将GC时间加到bucket.fill，但需满足bucket.fill最大为capacity。
	// 目前bucket.capcity的值是9s
	bucket  struct {
		// Invariants:
		// - fill >= 0
		// - capacity >= 0
		// - fill <= capacity
		fill, capacity uint64
	}
	// overflow is the cumulative amount of GC CPU time that we tried to fill the
	// bucket with but exceeded its capacity.
	//
	// 如果在GC时间与非GC时间在减去桶的剩余容量大于0时会增加到此计数。
	overflow uint64

	// gcEnabled is an internal copy of gcBlackenEnabled that determines
	// whether the limiter tracks total assist time.
	//
	// gcBlackenEnabled isn't used directly so as to keep this structure
	// unit-testable.
	gcEnabled bool

	// transitioning is true when the GC is in a STW and transitioning between
	// the mark and sweep phases.
	//
	// 在gcStart的STW状态通过调用 startGCTransition(true,now)，将此值设置为true。
	// 在gcStart的start the world之后通过调用 finishGCTransition(now)，将此值设置为false。
	//
	// 在gcMarkDown的STW状态通过调用 startGCTransition(false,now)，将此值设置为true。
	// 在gcMarkDown的start the world调用之前，通过调用 finishGCTransition(now)，将此值设置为false。
	//
	// 此值是一个保护标志，此值为true时不能调用 update。
	// startGCTransition 肯定会获取limiter 锁， finishGCTransition 肯定会释放limiter 锁。
	transitioning bool

	// assistTimePool is the accumulated assist time since the last update.
	// 在计算limiter是否开启时，算作GC time.
	assistTimePool atomic.Int64

	// idleMarkTimePool is the accumulated idle mark time since the last update.
	// 此字段目前没有用到。
	idleMarkTimePool atomic.Int64

	// idleTimePool is the accumulated time Ps spent on the idle list since the last update.
	// 在计算limiter是否开启时，非GC time会减去这个字段值。
	// 包括p在pidle list待的时间和p用于idle gc的时间。
	idleTimePool atomic.Int64

	// lastUpdate is the nanotime timestamp of the last time update was called.
	//
	// Updated under lock, but may be read concurrently.
	//
	// 上一次 update 调用时间，在计算时间窗口时会用当前时间减去此字段的值。
	// 总CPU时间为：total=(now-lastUpdate)*GOMAXPROCS
	// GC时间：total*gcBackgroundUtilization + assistTimePool 里的时间
	// 非GC时间：total - idleTimePool - GC时间。
	lastUpdate atomic.Int64

	// lastEnabledCycle is the GC cycle that last had the limiter enabled.
	// 最近开启limiter时，是第几轮GC，存储的是GC cycle的值。
	// 代码中将其设置为 memstats.numgc+1，+1是因为memstats.numgc在标记终止时才递增。
	// 与gcController.ncycle不同，其在扫描终止阶段时递增。
	lastEnabledCycle atomic.Uint32

	// nprocs is an internal copy of gomaxprocs, used to determine total available
	// CPU time.
	//
	// gomaxprocs isn't used directly so as to keep this structure unit-testable.
	nprocs int32

	// test indicates whether this instance of the struct was made for testing purposes.
	test bool
}

// limiting returns true if the CPU limiter is currently enabled, meaning the Go GC
// should take action to limit CPU utilization.
//
// It is safe to call concurrently with other operations.
func (l *gcCPULimiterState) limiting() bool {
	return l.enabled.Load()
}

// startGCTransition notifies the limiter of a GC transition.
//
// This call takes ownership of the limiter and disables all other means of
// updating the limiter. Release ownership by calling finishGCTransition.
//
// It is safe to call concurrently with other operations.
//
// 只在 gcStart 中调用(STW之后设置_GCmark之前)。
// 开启 gcCPULimiterState 统计，会在标记终止阶段停止统计。
func (l *gcCPULimiterState) startGCTransition(enableGC bool, now int64) {
	if !l.tryLock() {
		// This must happen during a STW, so we can't fail to acquire the lock.
		// If we did, something went wrong. Throw.
		throw("failed to acquire lock to start a GC transition")
	}
	if l.gcEnabled == enableGC {
		throw("transitioning GC to the same state as before?")
	}
	// Flush whatever was left between the last update and now.
	l.updateLocked(now)
	l.gcEnabled = enableGC
	l.transitioning = true
	// N.B. finishGCTransition releases the lock.
	//
	// We don't release here to increase the chance that if there's a failure
	// to finish the transition, that we throw on failing to acquire the lock.
}

// finishGCTransition notifies the limiter that the GC transition is complete
// and releases ownership of it. It also accumulates STW time in the bucket.
// now must be the timestamp from the end of the STW pause.
func (l *gcCPULimiterState) finishGCTransition(now int64) {
	if !l.transitioning {
		throw("finishGCTransition called without starting one?")
	}
	// Count the full nprocs set of CPU time because the world is stopped
	// between startGCTransition and finishGCTransition. Even though the GC
	// isn't running on all CPUs, it is preventing user code from doing so,
	// so it might as well be.
	if lastUpdate := l.lastUpdate.Load(); now >= lastUpdate {
		l.accumulate(0, (now-lastUpdate)*int64(l.nprocs))
	}
	l.lastUpdate.Store(now)
	l.transitioning = false
	l.unlock()
}

// gcCPULimiterUpdatePeriod dictates the maximum amount of wall-clock time
// we can go before updating the limiter.
const gcCPULimiterUpdatePeriod = 10e6 // 10ms

// needUpdate returns true if the limiter's maximum update period has been
// exceeded, and so would benefit from an update.
func (l *gcCPULimiterState) needUpdate(now int64) bool {
	return now-l.lastUpdate.Load() > gcCPULimiterUpdatePeriod
}

// addAssistTime notifies the limiter of additional assist time. It will be
// included in the next update.
func (l *gcCPULimiterState) addAssistTime(t int64) {
	l.assistTimePool.Add(t)
}

// addIdleTime notifies the limiter of additional time a P spent on the idle list. It will be
// subtracted from the total CPU time in the next update.
func (l *gcCPULimiterState) addIdleTime(t int64) {
	l.idleTimePool.Add(t)
}

// update updates the bucket given runtime-specific information. now is the
// current monotonic time in nanoseconds.
//
// This is safe to call concurrently with other operations, except *GCTransition.
func (l *gcCPULimiterState) update(now int64) {
	if !l.tryLock() {
		// We failed to acquire the lock, which means something else is currently
		// updating. Just drop our update, the next one to update will include
		// our total assist time.
		return
	}
	if l.transitioning {
		throw("update during transition")
	}
	l.updateLocked(now)
	l.unlock()
}

// updatedLocked is the implementation of update. l.lock must be held.
//
// 冲刷 l 中的统计信息和allp中现存的统计信息到l.bucket桶中。
func (l *gcCPULimiterState) updateLocked(now int64) {
	lastUpdate := l.lastUpdate.Load()
	if now < lastUpdate {
		// Defensively avoid overflow. This isn't even the latest update anyway.
		return
	}
	windowTotalTime := (now - lastUpdate) * int64(l.nprocs)
	l.lastUpdate.Store(now)

	// Drain the pool of assist time.
	assistTime := l.assistTimePool.Load()
	if assistTime != 0 {
		l.assistTimePool.Add(-assistTime)
	}

	// Drain the pool of idle time.
	idleTime := l.idleTimePool.Load()
	if idleTime != 0 {
		l.idleTimePool.Add(-idleTime)
	}

	if !l.test {
		// Consume time from in-flight events. Make sure we're not preemptible so allp can't change.
		//
		// The reason we do this instead of just waiting for those events to finish and push updates
		// is to ensure that all the time we're accounting for happened sometime between lastUpdate
		// and now. This dramatically simplifies reasoning about the limiter because we're not at
		// risk of extra time being accounted for in this window than actually happened in this window,
		// leading to all sorts of weird transient behavior.
		mp := acquirem()
		for _, pp := range allp {
			typ, duration := pp.limiterEvent.consume(now)
			switch typ {
			case limiterEventIdleMarkWork:
				fallthrough
			case limiterEventIdle:
				idleTime += duration
			case limiterEventMarkAssist:
				fallthrough
			case limiterEventScavengeAssist:
				assistTime += duration
			case limiterEventNone:
				break
			default:
				throw("invalid limiter event type found")
			}
		}
		releasem(mp)
	}

	// Compute total GC time.
	windowGCTime := assistTime
	if l.gcEnabled {
		windowGCTime += int64(float64(windowTotalTime) * gcBackgroundUtilization)
	}

	// Subtract out all idle time from the total time. Do this after computing
	// GC time, because the background utilization is dependent on the *real*
	// total time, not the total time after idle time is subtracted.
	//
	// Idle time is counted as any time that a P is on the P idle list plus idle mark
	// time. Idle mark workers soak up time that the application spends idle.
	//
	// On a heavily undersubscribed system, any additional idle time can skew GC CPU
	// utilization, because the GC might be executing continuously and thrashing,
	// yet the CPU utilization with respect to GOMAXPROCS will be quite low, so
	// the limiter fails to turn on. By subtracting idle time, we're removing time that
	// we know the application was idle giving a more accurate picture of whether
	// the GC is thrashing.
	//
	// Note that this can cause the limiter to turn on even if it's not needed. For
	// instance, on a system with 32 Ps but only 1 running goroutine, each GC will have
	// 8 dedicated GC workers. Assuming the GC cycle is half mark phase and half sweep
	// phase, then the GC CPU utilization over that cycle, with idle time removed, will
	// be 8/(8+2) = 80%. Even though the limiter turns on, though, assist should be
	// unnecessary, as the GC has way more CPU time to outpace the 1 goroutine that's
	// running.
	windowTotalTime -= idleTime

	l.accumulate(windowTotalTime-windowGCTime, windowGCTime)
}

// accumulate adds time to the bucket and signals whether the limiter is enabled.
//
// This is an internal function that deals just with the bucket. Prefer update.
// l.lock must be held.
//
// 此函数一般在 updateLocked(updateLocked 中计算出mutatorTime和gcTime然后调用此函数) 中调用，因为有锁的保护所以不会并发调用。
//
// 通过gcTimer与mutatorTime的差值和桶中剩余容量的比较。来判断是否开启limiter。
// 如果gcTime-mutatorTime为正，且差值大于桶的剩余容量则将桶设置为满态并开启limiter。
// 如果mutatorTime-gcTime为正，且差值大于桶已有时间则将桶置空并关闭Limiter。
// mutatorTime=gcTime则什么都不做。
// 其它情况根据情况增加或减少桶中的时间。
func (l *gcCPULimiterState) accumulate(mutatorTime, gcTime int64) {
	headroom := l.bucket.capacity - l.bucket.fill
	// enabled limiter是否已经处于开启状态。
	enabled := headroom == 0

	// Let's be careful about three things here:
	// 1. The addition and subtraction, for the invariants.
	// 2. Overflow.
	// 3. Excessive mutation of l.enabled, which is accessed
	//    by all assists, potentially more than once.
	change := gcTime - mutatorTime

	// Handle limiting case.
	// GC花费的时间大于非GC时间，如果它们的差值大于桶中剩余容量，
	// 将桶设置为满的状态。如果没有开启limiter则开启。
	if change > 0 && headroom <= uint64(change) {
		l.overflow += uint64(change) - headroom
		l.bucket.fill = l.bucket.capacity
		if !enabled {
			l.enabled.Store(true)
			l.lastEnabledCycle.Store(memstats.numgc + 1)
		}
		return
	}

	// Handle non-limiting cases.
	if change < 0 && l.bucket.fill <= uint64(-change) {
		// Bucket emptied.
		// GC时间-非GC时间是负值。则表示需要从桶中取出时间，
		// 绝对差值大于桶已有的时间直接将桶清空。
		l.bucket.fill = 0
	} else {
		// All other cases.
		// GC时间与非GC时间的差值为正时小于桶的剩余容量，为负时的绝对值小于桶中现有的时间。
		// 这两种情况都要修改桶中的时间。前者增加，后者减少绝对差值。
		l.bucket.fill -= uint64(-change)
	}
	if change != 0 && enabled {
		// 至此桶肯定未满：
		// 1 如果GC时间与非GC时间相等。则不需要改变limiter现有状态。
		// 2. 如果不相等且limiter已经开启，则关闭它。
		l.enabled.Store(false)
	}
}

// tryLock attempts to lock l. Returns true on success.
func (l *gcCPULimiterState) tryLock() bool {
	return l.lock.CompareAndSwap(0, 1)
}

// unlock releases the lock on l. Must be called if tryLock returns true.
func (l *gcCPULimiterState) unlock() {
	old := l.lock.Swap(0)
	if old != 1 {
		throw("double unlock")
	}
}

// capacityPerProc is the limiter's bucket capacity for each P in GOMAXPROCS.
const capacityPerProc = 1e9 // 1 second in nanoseconds

// resetCapacity updates the capacity based on GOMAXPROCS. Must not be called
// while the GC is enabled.
//
// It is safe to call concurrently with other operations.
func (l *gcCPULimiterState) resetCapacity(now int64, nprocs int32) {
	if !l.tryLock() {
		// This must happen during a STW, so we can't fail to acquire the lock.
		// If we did, something went wrong. Throw.
		throw("failed to acquire lock to reset capacity")
	}
	// Flush the rest of the time for this period.
	l.updateLocked(now)
	l.nprocs = nprocs

	l.bucket.capacity = uint64(nprocs) * capacityPerProc
	if l.bucket.fill > l.bucket.capacity {
		l.bucket.fill = l.bucket.capacity
		l.enabled.Store(true)
		l.lastEnabledCycle.Store(memstats.numgc + 1)
	} else if l.bucket.fill < l.bucket.capacity {
		l.enabled.Store(false)
	}
	l.unlock()
}

// limiterEventType indicates the type of an event occurring on some P.
//
// These events represent the full set of events that the GC CPU limiter tracks
// to execute its function.
//
// This type may use no more than limiterEventBits bits of information.
type limiterEventType uint8

const (
	limiterEventNone           limiterEventType = iota // None of the following events.
	// 会与 limiterEventIdle 合并到 gcCPULimiterState.idleTimePool 中。
	limiterEventIdleMarkWork                           // Refers to an idle mark worker (see gcMarkWorkerMode).
	// 会与 limiterEventScavengeAssist 合并到 gcCPULimiterState.assistTimePool 中。
	limiterEventMarkAssist                             // Refers to mark assist (see gcAssistAlloc).
	limiterEventScavengeAssist                         // Refers to a scavenge assist (see allocSpan).
	limiterEventIdle                                   // Refers to time a P spent on the idle list.

	limiterEventBits = 3
)

// limiterEventTypeMask is a mask for the bits in p.limiterEventStart that represent
// the event type. The rest of the bits of that field represent a timestamp.
const (
	// 0b111 < 61
	limiterEventTypeMask  = uint64((1<<limiterEventBits)-1) << (64 - limiterEventBits)
	limiterEventStampNone = limiterEventStamp(0)
)

// limiterEventStamp is a nanotime timestamp packed with a limiterEventType.
type limiterEventStamp uint64

// makeLimiterEventStamp creates a new stamp from the event type and the current timestamp.
func makeLimiterEventStamp(typ limiterEventType, now int64) limiterEventStamp {
	return limiterEventStamp(uint64(typ)<<(64-limiterEventBits) | (uint64(now) &^ limiterEventTypeMask))
}

// duration computes the difference between now and the start time stored in the stamp.
//
// Returns 0 if the difference is negative, which may happen if now is stale or if the
// before and after timestamps cross a 2^(64-limiterEventBits) boundary.
func (s limiterEventStamp) duration(now int64) int64 {
	// The top limiterEventBits bits of the timestamp are derived from the current time
	// when computing a duration.
	// 0b111 < 61
	// 使用now的高3位作为start的高3位，因为执行duration的跨度不可能超过2^61纳秒。
	start := int64((uint64(now) & limiterEventTypeMask) | (uint64(s) &^ limiterEventTypeMask))
	if now < start {
		return 0
	}
	return now - start
}

// type extracts the event type from the stamp.
func (s limiterEventStamp) typ() limiterEventType {
	return limiterEventType(s >> (64 - limiterEventBits))
}

// limiterEvent represents tracking state for an event tracked by the GC CPU limiter.
type limiterEvent struct {
	stamp atomic.Uint64 // Stores a limiterEventStamp.
}

// start begins tracking a new limiter event of the current type. If an event
// is already in flight, then a new event cannot begin because the current time is
// already being attributed to that event. In this case, this function returns false.
// Otherwise, it returns true.
//
// The caller must be non-preemptible until at least stop is called or this function
// returns false. Because this is trying to measure "on-CPU" time of some event, getting
// scheduled away during it can mean that whatever we're measuring isn't a reflection
// of "on-CPU" time. The OS could deschedule us at any time, but we want to maintain as
// close of an approximation as we can.
//
// 不能被抢占，因为这样才能最接近原始的cpu时间。当然这种不能抢占不能避免系统调度造成的闲置时间。
// 只能防止Go本身的调度。
func (e *limiterEvent) start(typ limiterEventType, now int64) bool {
	if limiterEventStamp(e.stamp.Load()).typ() != limiterEventNone {
		return false
	}
	e.stamp.Store(uint64(makeLimiterEventStamp(typ, now)))
	return true
}

// consume acquires the partial event CPU time from any in-flight event.
// It achieves this by storing the current time as the new event time.
//
// Returns the type of the in-flight event, as well as how long it's currently been
// executing for. Returns limiterEventNone if no event is active.
func (e *limiterEvent) consume(now int64) (typ limiterEventType, duration int64) {
	// Read the limiter event timestamp and update it to now.
	for {
		old := limiterEventStamp(e.stamp.Load())
		typ = old.typ()
		if typ == limiterEventNone {
			// There's no in-flight event, so just push that up.
			return
		}
		duration = old.duration(now)
		if duration == 0 {
			// We might have a stale now value, or this crossed the
			// 2^(64-limiterEventBits) boundary in the clock readings.
			// Just ignore it.
			return limiterEventNone, 0
		}
		new := makeLimiterEventStamp(typ, now)
		if e.stamp.CompareAndSwap(uint64(old), uint64(new)) {
			break
		}
	}
	return
}

// stop stops the active limiter event. Throws if the
//
// The caller must be non-preemptible across the event. See start as to why.
func (e *limiterEvent) stop(typ limiterEventType, now int64) {
	var stamp limiterEventStamp
	for {
		stamp = limiterEventStamp(e.stamp.Load())
		if stamp.typ() != typ {
			print("runtime: want=", typ, " got=", stamp.typ(), "\n")
			throw("limiterEvent.stop: found wrong event in p's limiter event slot")
		}
		if e.stamp.CompareAndSwap(uint64(stamp), uint64(limiterEventStampNone)) {
			break
		}
	}
	duration := stamp.duration(now)
	if duration == 0 {
		// It's possible that we're missing time because we crossed a
		// 2^(64-limiterEventBits) boundary between the start and end.
		// In this case, we're dropping that information. This is OK because
		// at worst it'll cause a transient hiccup that will quickly resolve
		// itself as all new timestamps begin on the other side of the boundary.
		// Such a hiccup should be incredibly rare.
		return
	}
	// Account for the event.
	// 将本地缓存的GC时间冲刷到对应的全局缓存池。
	switch typ {
	case limiterEventIdleMarkWork:
		gcCPULimiter.addIdleTime(duration)
	case limiterEventIdle:
		gcCPULimiter.addIdleTime(duration)
		sched.idleTime.Add(duration)
	case limiterEventMarkAssist:
		fallthrough
	case limiterEventScavengeAssist:
		gcCPULimiter.addAssistTime(duration)
	default:
		throw("limiterEvent.stop: invalid limiter event type found")
	}
}
