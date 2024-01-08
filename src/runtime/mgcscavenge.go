// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Scavenging free pages.
//
// This file implements scavenging (the release of physical pages backing mapped
// memory) of free and unused pages in the heap as a way to deal with page-level
// fragmentation and reduce the RSS of Go applications.
//
// Scavenging in Go happens on two fronts: there's the background
// (asynchronous) scavenger and the heap-growth (synchronous) scavenger.
//
// The former happens on a goroutine much like the background sweeper which is
// soft-capped at using scavengePercent of the mutator's time, based on
// order-of-magnitude estimates of the costs of scavenging. The background
// scavenger's primary goal is to bring the estimated heap RSS of the
// application down to a goal.
//
// Before we consider what this looks like, we need to split the world into two
// halves. One in which a memory limit is not set, and one in which it is.
//
// For the former, the goal is defined as:
//   (retainExtraPercent+100) / 100 * (heapGoal / lastHeapGoal) * lastHeapInUse
//
// Essentially, we wish to have the application's RSS track the heap goal, but
// the heap goal is defined in terms of bytes of objects, rather than pages like
// RSS. As a result, we need to take into account for fragmentation internal to
// spans. heapGoal / lastHeapGoal defines the ratio between the current heap goal
// and the last heap goal, which tells us by how much the heap is growing and
// shrinking. We estimate what the heap will grow to in terms of pages by taking
// this ratio and multiplying it by heapInUse at the end of the last GC, which
// allows us to account for this additional fragmentation. Note that this
// procedure makes the assumption that the degree of fragmentation won't change
// dramatically over the next GC cycle. Overestimating the amount of
// fragmentation simply results in higher memory use, which will be accounted
// for by the next pacing up date. Underestimating the fragmentation however
// could lead to performance degradation. Handling this case is not within the
// scope of the scavenger. Situations where the amount of fragmentation balloons
// over the course of a single GC cycle should be considered pathologies,
// flagged as bugs, and fixed appropriately.
//
// An additional factor of retainExtraPercent is added as a buffer to help ensure
// that there's more unscavenged memory to allocate out of, since each allocation
// out of scavenged memory incurs a potentially expensive page fault.
//
// If a memory limit is set, then we wish to pick a scavenge goal that maintains
// that memory limit. For that, we look at total memory that has been committed
// (memstats.mappedReady) and try to bring that down below the limit. In this case,
// we want to give buffer space in the *opposite* direction. When the application
// is close to the limit, we want to make sure we push harder to keep it under, so
// if we target below the memory limit, we ensure that the background scavenger is
// giving the situation the urgency it deserves.
//
// In this case, the goal is defined as:
//    (100-reduceExtraPercent) / 100 * memoryLimit
//
// We compute both of these goals, and check whether either of them have been met.
// The background scavenger continues operating as long as either one of the goals
// has not been met.
//
// The goals are updated after each GC.
//
// The synchronous heap-growth scavenging happens whenever the heap grows in
// size, for some definition of heap-growth. The intuition behind this is that
// the application had to grow the heap because existing fragments were
// not sufficiently large to satisfy a page-level memory allocation, so we
// scavenge those fragments eagerly to offset the growth in RSS that results.

package runtime

import (
	"internal/goos"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// The background scavenger is paced according to these parameters.
	//
	// scavengePercent represents the portion of mutator time we're willing
	// to spend on scavenging in percent.
	scavengePercent = 1 // 1%

	// retainExtraPercent represents the amount of memory over the heap goal
	// that the scavenger should keep as a buffer space for the allocator.
	// This constant is used when we do not have a memory limit set.
	//
	// The purpose of maintaining this overhead is to have a greater pool of
	// unscavenged memory available for allocation (since using scavenged memory
	// incurs an additional cost), to account for heap fragmentation and
	// the ever-changing layout of the heap.
	retainExtraPercent = 10

	// reduceExtraPercent represents the amount of memory under the limit
	// that the scavenger should target. For example, 5 means we target 95%
	// of the limit.
	//
	// The purpose of shooting lower than the limit is to ensure that, once
	// close to the limit, the scavenger is working hard to maintain it. If
	// we have a memory limit set but are far away from it, there's no harm
	// in leaving up to 100-retainExtraPercent live, and it's more efficient
	// anyway, for the same reasons that retainExtraPercent exists.
	reduceExtraPercent = 5

	// maxPagesPerPhysPage is the maximum number of supported runtime pages per
	// physical page, based on maxPhysPageSize.
	maxPagesPerPhysPage = maxPhysPageSize / pageSize

	// scavengeCostRatio is the approximate ratio between the costs of using previously
	// scavenged memory and scavenging memory.
	//
	// For most systems the cost of scavenging greatly outweighs the costs
	// associated with using scavenged memory, making this constant 0. On other systems
	// (especially ones where "sysUsed" is not just a no-op) this cost is non-trivial.
	//
	// This ratio is used as part of multiplicative factor to help the scavenger account
	// for the additional costs of using scavenged memory in its pacing.
	//
	// using scavenged memory/scavenging
	// linux 此值是0
	scavengeCostRatio = 0.7 * (goos.IsDarwin + goos.IsIos)
)

// heapRetained returns an estimate of the current heap RSS.
func heapRetained() uint64 {
	return gcController.heapInUse.load() + gcController.heapFree.load()
}

// gcPaceScavenger updates the scavenger's pacing, particularly
// its rate and RSS goal. For this, it requires the current heapGoal,
// and the heapGoal for the previous GC cycle.
//
// The RSS goal is based on the current heap goal with a small overhead
// to accommodate non-determinism in the allocator.
//
// The pacing is based on scavengePageRate, which applies to both regular and
// huge pages. See that constant for more information.
//
// Must be called whenever GC pacing is updated.
//
// mheap_.lock must be held or the world must be stopped.
//
// 目前只被被 gcMarkTermination -> gcControllerCommit 函数所调用(标记终止阶段刚刚结束此时GC的_GCoff状态)。
// ------
// 根据GC的一些统计数据来设置 scavenge 的执行步调。
// 此函数会设置 scavenge.gcPercentGoal 和 scavenge.memoryLimitGoal 的值。
// 如果它们的值被设置为 ^uint64(0) 则表示现在不需要进行scavenge。
//
// 参数:
// 1. heapGoal 下一次GC触发时的目标值(因为标记已经结束)。
// 2. 本轮GC触发的目标值。
// 3. memoryLimit 默认值是0如果没有开启MemoryLimit机制的话。
func gcPaceScavenger(memoryLimit int64, heapGoal, lastHeapGoal uint64) {
	assertWorldStoppedOrLockHeld(&mheap_.lock)

	// As described at the top of this file, there are two scavenge goals here: one
	// for gcPercent and one for memoryLimit. Let's handle the latter first because
	// it's simpler.

	// We want to target retaining (100-reduceExtraPercent)% of the heap.
	//
	// memoryLimitGoal=memroyLimit*95
	println("...",memoryLimit)
	memoryLimitGoal := uint64(float64(memoryLimit) * (100.0 - reduceExtraPercent))
	println("memoryLimitGoal",memoryLimitGoal)

	// mappedReady is comparable to memoryLimit, and represents how much total memory
	// the Go runtime has committed now (estimated).
	mappedReady := gcController.mappedReady.Load()

	// If we're below the goal already indicate that we don't need the background
	// scavenger for the memory limit. This may seems worrisome at first, but note
	// that the allocator will assist the background scavenger in the face of a memory
	// limit, so we'll be safe even if we stop the scavenger when we shouldn't have.
	//
	println("mappedReady-memoryLimitGoal",mappedReady,memoryLimitGoal)
	if mappedReady <= memoryLimitGoal {
		println("11111111111111")
		// 如果commit的内存大小小于MEMORYLIMIT，则不需要后台scavenger为MEMORYLIMIT服务。
		scavenge.memoryLimitGoal.Store(^uint64(0))
	} else {
		scavenge.memoryLimitGoal.Store(memoryLimitGoal)
	}

	// Now handle the gcPercent goal.

	// If we're called before the first GC completed, disable scavenging.
	// We never scavenge before the 2nd GC cycle anyway (we don't have enough
	// information about the heap yet) so this is fine, and avoids a fault
	// or garbage data later.
	if lastHeapGoal == 0 {
		scavenge.gcPercentGoal.Store(^uint64(0))
		return
	}
	// Compute our scavenging goal.
	goalRatio := float64(heapGoal) / float64(lastHeapGoal)
	gcPercentGoal := uint64(float64(memstats.lastHeapInUse) * goalRatio)
	// Add retainExtraPercent overhead to retainedGoal. This calculation
	// looks strange but the purpose is to arrive at an integer division
	// (e.g. if retainExtraPercent = 12.5, then we get a divisor of 8)
	// that also avoids the overflow from a multiplication.
	gcPercentGoal += gcPercentGoal / (1.0 / (retainExtraPercent / 100.0))
	// Align it to a physical page boundary to make the following calculations
	// a bit more exact.
	gcPercentGoal = (gcPercentGoal + uint64(physPageSize) - 1) &^ (uint64(physPageSize) - 1)

	// Represents where we are now in the heap's contribution to RSS in bytes.
	//
	// Guaranteed to always be a multiple of physPageSize on systems where
	// physPageSize <= pageSize since we map new heap memory at a size larger than
	// any physPageSize and released memory in multiples of the physPageSize.
	//
	// However, certain functions recategorize heap memory as other stats (e.g.
	// stacks) and this happens in multiples of pageSize, so on systems
	// where physPageSize > pageSize the calculations below will not be exact.
	// Generally this is OK since we'll be off by at most one regular
	// physical page.
	heapRetainedNow := heapRetained()

	// If we're already below our goal, or within one page of our goal, then indicate
	// that we don't need the background scavenger for maintaining a memory overhead
	// proportional to the heap goal.
	if heapRetainedNow <= gcPercentGoal || heapRetainedNow-gcPercentGoal < uint64(physPageSize) {
		scavenge.gcPercentGoal.Store(^uint64(0))
	} else {
		scavenge.gcPercentGoal.Store(gcPercentGoal)
	}
}

var scavenge struct {
	// gcPercentGoal is the amount of retained heap memory (measured by
	// heapRetained) that the runtime will try to maintain by returning
	// memory to the OS. This goal is derived from gcController.gcPercent
	// by choosing to retain enough memory to allocate heap memory up to
	// the heap goal.
	//
	// 只会在gcPaceScavenger函数中被设置，其在每一轮GC标记终止时被调用。
	// 设置时如果当前heap retained的内存大小在此值之下，便会将此值设置为^uint64(0)
	// 即直到下一次设置此值之前都不需要进行 gcPercent scavenge。
	gcPercentGoal atomic.Uint64

	// memoryLimitGoal is the amount of memory retained by the runtime (
	// measured by gcController.mappedReady) that the runtime will try to
	// maintain by returning memory to the OS. This goal is derived from
	// gcController.memoryLimit by choosing to target the memory limit or
	// some lower target to keep the scavenger working.
	//
	// 只会在gcPaceScavenger函数中被设置，其在每一轮GC标记终止时被调用。
	// 设置时如果当前commit的内存大小在此值之下，便会将此值设置为^uint64(0)
	// 即直到下一次设置此值之前都不需要进行 gcPercent scavenge。
	memoryLimitGoal atomic.Uint64

	// assistTime is the time spent by the allocator scavenging in the last GC cycle.
	//
	// This is reset once a GC cycle ends.
	assistTime atomic.Int64

	// backgroundTime is the time spent by the background scavenger in the last GC cycle.
	//
	// This is reset once a GC cycle ends.
	//
	//统计后台scavenge协程的工作时长
	backgroundTime atomic.Int64
}

const (
	// It doesn't really matter what value we start at, but we can't be zero, because
	// that'll cause divide-by-zero issues. Pick something conservative which we'll
	// also use as a fallback.
	//
	// 保守值，即让bg scavenge尽量少的工作。
	// 也是scavenger.sleepRatio的最小值。
	startingScavSleepRatio = 0.001

	// Spend at least 1 ms scavenging, otherwise the corresponding
	// sleep time to maintain our desired utilization is too low to
	// be reliable.
	//
	// 在根据工作时间计算睡眠时间时，如果工作时间小于此值，那么就取此值作为工作时间。
	minScavWorkTime = 1e6
)

// Sleep/wait state of the background scavenger.
// 全局后台scavenge变量。
var scavenger scavengerState

type scavengerState struct {
	// lock protects all fields below.
	lock mutex

	// g is the goroutine the scavenger is bound to.
	//
	// scavenge 后台只工作协程设置到此字段，然后其它协程(sysmon/gc清扫结束时也会唤醒)通过全局变量
	// scavenger 来唤醒此后台scavenge工作者。
	g *g

	// parked is whether or not the scavenger is parked.
	//
	// wake 方法的包含字段，多次调用wake方法不会产生副作用。
	parked bool

	// timer is the timer used for the scavenger to sleep.
	//
	// 实现后台scavenge休眠机制。
	timer *timer

	// sysmonWake signals to sysmon that it should wake the scavenger.
	//
	// 指导sysmon 是否需要在适当时机唤醒后台scavenge工作者。
	// 为1表示需要，为0表示不需要。
	sysmonWake atomic.Uint32

	// targetCPUFraction is the target CPU overhead for the scavenger.
	//
	// 目前此字段没有别使用到。
	targetCPUFraction float64

	// sleepRatio is the ratio of time spent doing scavenging work to
	// time spent sleeping. This is used to decide how long the scavenger
	// should sleep for in between batches of work. It is set by
	// critSleepController in order to maintain a CPU overhead of
	// targetCPUFraction.
	//
	// Lower means more sleep, higher means more aggressive scavenging.
	//
	// 执行scavenge的时间与休眠时间的比率。
	// 比如一次 run 调用花费了8ms，那么接下来就需要休眠8ms/sleepRatio。
	// 默认值/保守值是0.001，在sleep方法中会进行有条件更新，范围是[0.001,1000]
	sleepRatio float64

	// sleepController controls sleepRatio.
	//
	// See sleepRatio for more details.
	//
	// 在sleep函数调用中负责计数 sleepRatio 的值。
	sleepController piController

	// cooldown is the time left in nanoseconds during which we avoid
	// using the controller and we hold sleepRatio at a conservative
	// value. Used if the controller's assumptions fail to hold.
	//
	// 在sleepController 计算sleepRatio 失败时会将此字段设置为5s，sleepRatio设置为0.001。
	// 5s表示在接下来的5s中都不需要向 sleepController 请求新的 sleepRatio。
	controllerCooldown int64

	// printControllerReset instructs printScavTrace to signal that
	// the controller was reset.
	//
	// 在 sleepController 计算 sleepRatio 失败时，会将此字段设置为true，
	// 如果在清扫结束时，开启了scavenge debug，则会调试信息末尾追加：
	// print(" [controller reset]")
	printControllerReset bool

	// sleepStub is a stub used for testing to avoid actually having
	// the scavenger sleep.
	//
	// Unlike the other stubs, this is not populated if left nil
	// Instead, it is called when non-nil because any valid implementation
	// of this function basically requires closing over this scavenger
	// state, and allocating a closure is not allowed in the runtime as
	// a matter of policy.
	//
	// 此字段非测试时肯定是nil，返回值是实际休眠的时间，因为有可能被提前唤醒。
	sleepStub func(n int64) int64

	// scavenge is a function that scavenges n bytes of memory.
	// Returns how many bytes of memory it actually scavenged, as
	// well as the time it took in nanoseconds. Usually mheap.pages.scavenge
	// with nanotime called around it, but stubbed out for testing.
	// Like mheap.pages.scavenge, if it scavenges less than n bytes of
	// memory, the caller may assume the heap is exhausted of scavengable
	// memory for now.
	//
	// If this is nil, it is populated with the real thing in init.
	//
	// 此字段肯定不为nil。用于执行具体的 scavenge 任务。
	// 其会调用 pageAlloc.scavenge
	scavenge func(n uintptr) (uintptr, int64)

	// shouldStop is a callback called in the work loop and provides a
	// point that can force the scavenger to stop early, for example because
	// the scavenge policy dictates too much has been scavenged already.
	//
	// If this is nil, it is populated with the real thing in init.
	//
	// 不会是nil，
	//
	// 满足以下条件之一此函数返回true。
	// 1. 当前堆中执行过sysUsed的内存 <= scavenge.gcPercentGoal
	// 2. 所有commit(sysUsed)的所有内存 <= MEMORYLIMIT设置的内存
	shouldStop func() bool

	// gomaxprocs returns the current value of gomaxprocs. Stub for testing.
	//
	// If this is nil, it is populated with the real thing in init.
	// 返回GOMAXPROCS
	gomaxprocs func() int32
}

// init initializes a scavenger state and wires to the current G.
//
// Must be called from a regular goroutine that can allocate.
//
// 为后台scavenge工作，做一些初始化工作。
// 此函数只会被调用一次在runtime.main中被调用。
func (s *scavengerState) init() {
	if s.g != nil {
		throw("scavenger state is already wired")
	}
	lockInit(&s.lock, lockRankScavenge)
	// s.g是后台执行scavenge的协程。
	// 将当前协程保存到全局变量 scavenge 上。方便唤醒它的协程操作。
	s.g = getg()

	// 睡眠唤醒时间器。
	s.timer = new(timer)
	s.timer.arg = s
	s.timer.f = func(s any, _ uintptr) {
		// 休眠时间到了执行的函数，唤醒后台scavenge协程。
		s.(*scavengerState).wake()
	}

	// input: fraction of CPU time actually used.
	// setpoint: ideal CPU fraction.
	// output: ratio of time worked to time slept (determines sleep time).
	//
	// The output of this controller is somewhat indirect to what we actually
	// want to achieve: how much time to sleep for. The reason for this definition
	// is to ensure that the controller's outputs have a direct relationship with
	// its inputs (as opposed to an inverse relationship), making it somewhat
	// easier to reason about for tuning purposes.
	//
	// 输入scavenge耗费的cpu资源。
	// 输出工作时间/休眠时间应有的比率，参与计算接下来的睡眠时间。
	s.sleepController = piController{
		// Tuned loosely via Ziegler-Nichols process.
		kp: 0.3375,
		ti: 3.2e6,
		tt: 1e9, // 1 second reset time.

		// These ranges seem wide, but we want to give the controller plenty of
		// room to hunt for the optimal value.
		min: 0.001,  // 1:1000
		max: 1000.0, // 1000:1
	}
	// 初始值 0.001
	s.sleepRatio = startingScavSleepRatio

	// Install real functions if stubs aren't present.
	if s.scavenge == nil {
		// 执行scavenge任务的函数。
		// 返回值：1. 此次scavenge回收的字节数 2. 此次回收花费的时间
		s.scavenge = func(n uintptr) (uintptr, int64) {
			start := nanotime()
			r := mheap_.pages.scavenge(n, nil)
			end := nanotime()
			if start >= end {
				return r, 0
			}
			scavenge.backgroundTime.Add(end - start)
			return r, end - start
		}
	}
	if s.shouldStop == nil {
		// 用于指示是否继续进行scavenge。
		s.shouldStop = func() bool {
			// If background scavenging is disabled or if there's no work to do just stop.
			//
			// 满足以下条件之一此函数返回true。
			// 1. 当前堆中执行过sysUsed的内存 <= scavenge.gcPercentGoal
			// 2. 所有commit(sysUsed)的所有内存 <= MEMORYLIMIT设置的内存
			// println("scavenge.memoryLimitGoal.Load()",scavenge.memoryLimitGoal.Load())
			// println("gcController.mappedReady.Load()",gcController.mappedReady.Load())
			return heapRetained() <= scavenge.gcPercentGoal.Load() &&
				(!go119MemoryLimitSupport ||
					gcController.mappedReady.Load() <= scavenge.memoryLimitGoal.Load())
		}
	}
	if s.gomaxprocs == nil {
		s.gomaxprocs = func() int32 {
			return gomaxprocs
		}
	}
}

// park parks the scavenger goroutine.
func (s *scavengerState) park() {
	lock(&s.lock)
	if getg() != s.g {
		throw("tried to park scavenger from another goroutine")
	}
	s.parked = true
	goparkunlock(&s.lock, waitReasonGCScavengeWait, traceEvGoBlock, 2)
}

// ready signals to sysmon that the scavenger should be awoken.
//
// 在清扫结束时(没有更多任务可清扫，但有可能还有其它清扫者在执行)调用此函数。
// 指示监控线程可以适时唤醒后台scavenge工作者。
func (s *scavengerState) ready() {
	s.sysmonWake.Store(1)
}

// 唤醒 scavenger ，按需进页归还给操作系统。
// 会被以下3种机制唤醒：
// 1. sysmon
// 2. 每轮GC在完成上一轮GC清扫工作后会调用此函数， gcStart -> finishsweep_m()
// 3. scavenger 自己睡眠一段时间后，会在关联的 timer.f 中调用此函数。
//
// wake immediately unparks the scavenger if necessary.
//
// Safe to run without a P.
func (s *scavengerState) wake() {
	lock(&s.lock)
	// 避免重复调用wake()产生副作用。
	if s.parked {
		// Unset sysmonWake, since the scavenger is now being awoken.
		s.sysmonWake.Store(0)

		// s.parked is unset to prevent a double wake-up.
		s.parked = false

		// Ready the goroutine by injecting it. We use injectglist instead
		// of ready or goready in order to allow us to run this function
		// without a P. injectglist also avoids placing the goroutine in
		// the current P's runnext slot, which is desirable to prevent
		// the scavenger from interfering with user goroutine scheduling
		// too much.
		var list gList
		list.push(s.g)
		injectglist(&list)
	}
	unlock(&s.lock)
}

// sleep puts the scavenger to sleep based on the amount of time that it worked
// in nanoseconds.
//
// Note that this function should only be called by the scavenger.
//
// The scavenger may be woken up earlier by a pacing change, and it may not go
// to sleep at all if there's a pending pacing change.
// --------------------------------------------------------------
// 每当 scavenger从停靠中被唤醒以调用run执行scavenge，
// run调用会返回一个名为workTime的参数。
// workTime是调用scavenger.run中执行scavenge花费的时间。
// scavenger.sleep会根据输入的工作时间(workTime)计算接下来需要sleep的时间。
func (s *scavengerState) sleep(worked float64) {
	lock(&s.lock)
	if getg() != s.g {
		throw("tried to sleep scavenger from another goroutine")
	}

	// worked < 1ms，最短输入时间1ms。
	if worked < minScavWorkTime {
		// This means there wasn't enough work to actually fill up minScavWorkTime.
		// That's fine; we shouldn't try to do anything with this information
		// because it's going result in a short enough sleep request that things
		// will get messy. Just assume we did at least this much work.
		// All this means is that we'll sleep longer than we otherwise would have.
		worked = minScavWorkTime
	}

	// Multiply the critical time by 1 + the ratio of the costs of using
	// scavenged memory vs. scavenging memory. This forces us to pay down
	// the cost of reusing this memory eagerly by sleeping for a longer period
	// of time and scavenging less frequently. More concretely, we avoid situations
	// where we end up scavenging so often that we hurt allocation performance
	// because of the additional overheads of using scavenged memory.
	//
	// linux 平台下 scavengeCostRatio 为0
	// scavengeCostRatio 表示直接使用scavenged过的页面和执行scavenge的成本比。
	//
	// scavengeCostRatio is the approximate ratio between the costs of using previously
	// scavenged memory and scavenging memory.
	//
	// 如果使用已scavenged页面代价较高，则因放缓scavenge的步伐。
	worked *= 1 + scavengeCostRatio

	// sleepTime is the amount of time we're going to sleep, based on the amount
	// of time we worked, and the sleepRatio.
	//
	// worked 最小时间是1e6
	// s.sleepRatio 初始值/回退值是 0.001
	// s.sleepRatio 值最小，睡眠的时间越长。
	sleepTime := int64(worked / s.sleepRatio)

	var slept int64
	if s.sleepStub == nil {
		// 除了测试，s.sleepStub就是nil。
		//
		// Set the timer.
		//
		// This must happen here instead of inside gopark
		// because we can't close over any variables without
		// failing escape analysis.
		start := nanotime()
		resetTimer(s.timer, start+sleepTime)

		// Mark ourselves as asleep and go to sleep.
		// 避免重复调用wake()产生副作用。
		s.parked = true
		// 必须持有锁，否则tiemr.f在停靠之前被执行时，会造成混乱。
		// 因为tiemr.f里首先要获得此锁。
		goparkunlock(&s.lock, waitReasonSleep, traceEvGoSleep, 2)

		// How long we actually slept for.
		//
		// 因为除了timer还有其它情况下会被唤醒。
		slept = nanotime() - start

		lock(&s.lock)
		// Stop the timer here because s.wake is unable to do it for us.
		// We don't really care if we succeed in stopping the timer. One
		// reason we might fail is that we've already woken up, but the timer
		// might be in the process of firing on some other P; essentially we're
		// racing with it. That's totally OK. Double wake-ups are perfectly safe.
		// 为何要持有锁？？？
		//
		// 已经唤醒了就清除timer无论这次是否因为timer而被唤醒。
		stopTimer(s.timer)
		unlock(&s.lock)
	} else {
		unlock(&s.lock)
		slept = s.sleepStub(sleepTime)
	}

	// Stop here if we're cooling down from the controller.
	// controllerCooldown大于0，则不必理会sleepController。
	// 使用保守的 sleepRatio，0.001。
	if s.controllerCooldown > 0 {
		// 可以继续run。
		// worked and slept aren't exact measures of time, but it's OK to be a bit
		// sloppy here. We're just hoping we're avoiding some transient bad behavior.
		t := slept + int64(worked)
		if t > s.controllerCooldown {
			s.controllerCooldown = 0
		} else {
			s.controllerCooldown -= t
		}
		return
	}

	// idealFraction is the ideal % of overall application CPU time that we
	// spend scavenging.
	idealFraction := float64(scavengePercent) / 100.0

	// Calculate the CPU time spent.
	//
	// This may be slightly inaccurate with respect to GOMAXPROCS, but we're
	// recomputing this often enough relative to GOMAXPROCS changes in general
	// (it only changes when the world is stopped, and not during a GC) that
	// that small inaccuracy is in the noise.
	cpuFraction := worked / ((float64(slept) + worked) * float64(s.gomaxprocs()))

	// Update the critSleepRatio, adjusting until we reach our ideal fraction.
	var ok bool
	// next 方法根据这3个输入值计数sleepRation。
	// 如果计数出错，比如值溢出/结果非数值等问题，ok返回值为false，回退到保守/默认的sleepRatio(0.001)。
	// s.sleepRation这个返回值的取值范围是[s.sleepCOntroller.min,max]=[0.001,1000]
	//
	// 如果idealFraction比cpuFraction越来越大(差值)，则返回值sleepRatioin会越来越大。
	// 如果idealFraction比cupFraction越来越小(差值)，则返回值sleepRatioion会越来越小。
	s.sleepRatio, ok = s.sleepController.next(cpuFraction, idealFraction, float64(slept)+worked)
	if !ok {
		// next方法计算出错，回退到 startingScavSleepRatio。
		// The core assumption of the controller, that we can get a proportional
		// response, broke down. This may be transient, so temporarily switch to
		// sleeping a fixed, conservative amount.
		//
		// 回退到0.001
		s.sleepRatio = startingScavSleepRatio
		// scavenge工作时长和休眠时长没超过5s时，一直使用startingScavSleepRatio。
		s.controllerCooldown = 5e9 // 5 seconds.

		// Signal the scav trace printer to output this.
		// 打印scavenge调试信息时会输出时会在末尾添加以下字样：
		// [controller reset]
		s.controllerFailed()
	}
}

// controllerFailed indicates that the scavenger's scheduling
// controller failed.
func (s *scavengerState) controllerFailed() {
	lock(&s.lock)
	s.printControllerReset = true
	unlock(&s.lock)
}

// run is the body of the main scavenging loop.
//
// Returns the number of bytes released and the estimated time spent
// releasing those bytes.
//
// Must be run on the scavenger goroutine.
//
// 执行后台清扫任务的函数。
// 每当被唤醒时就会执行此函数。
// 每次 s.scavenge 调用回收64KB的内存
// 返回条件，满足以下条件之一返回：
// 1. scavenge 工作总时长超过了1ms
// 2. 没有更多页面可回收。
// 3. 当前堆大小没有超过目标堆。
func (s *scavengerState) run() (released uintptr, worked float64) {
	lock(&s.lock)
	if getg() != s.g {
		throw("tried to run scavenger from another goroutine")
	}
	unlock(&s.lock)

	// 后台scavenge最短持续时间是1ms。
	// 每次回收64KB的内存，直到累计工作时间达到1ms。
	for worked < minScavWorkTime {
		// If something from outside tells us to stop early, stop.
		// 如果当前堆大小没有达到目标，便不再执行scavenge。
		if s.shouldStop() {
			break
		}

		// scavengeQuantum is the amount of memory we try to scavenge
		// in one go. A smaller value means the scavenger is more responsive
		// to the scheduler in case of e.g. preemption. A larger value means
		// that the overheads of scavenging are better amortized, so better
		// scavenging throughput.
		//
		// The current value is chosen assuming a cost of ~10µs/physical page
		// (this is somewhat pessimistic), which implies a worst-case latency of
		// about 160µs for 4(应该是64) KiB physical pages. The current value is biased
		// toward latency over throughput.
		//
		// 一次scavenge任务需清扫的字节数。
		// 如果此值太大会导致不能即使响应抢占，因为回收工作是在系统栈中进行的。
		// 如果此值太小会导致此后台scavenge任务吞吐量降低。
		// 这里选择了64KB，即完成这些任务量保守需要160us。
		const scavengeQuantum = 64 << 10

		// Accumulate the amount of time spent scavenging.
		// r 此次scavenge回收的字节数。
		// duration 此次scavenge持续时长。
		r, duration := s.scavenge(scavengeQuantum)

		// On some platforms we may see end >= start if the time it takes to scavenge
		// memory is less than the minimum granularity of its clock (e.g. Windows) or
		// due to clock bugs.
		//
		// In this case, just assume scavenging takes 10 µs per regular physical page
		// (determined empirically), and conservatively ignore the impact of huge pages
		// on timing.
		//
		// 一些平台时间精度问题或者bug导致duration等于0，则根据r计算出回收的页面数，
		// 然后按回收一页花费10us时长，修正duration。
		const approxWorkedNSPerPhysicalPage = 10e3
		if duration == 0 {
			worked += approxWorkedNSPerPhysicalPage * float64(r/physPageSize)
		} else {
			// TODO(mknyszek): If duration is small compared to worked, it could be
			// rounded down to zero. Probably not a problem in practice because the
			// values are all within a few orders of magnitude of each other but maybe
			// worth worrying about.
			//
			// 累加工作时间。
			worked += float64(duration)
		}
		// 累加回收字节数。
		released += r

		// scavenge does not return until it either finds the requisite amount of
		// memory to scavenge, or exhausts the heap. If we haven't found enough
		// to scavenge, then the heap must be exhausted.
		//
		// 如果某次scavenge没回收scavengeQuantum大小的字节，那么可以结束scavenge任务了，无论
		// 总工作时长有没有满足。
		if r < scavengeQuantum {
			break
		}
		// When using fake time just do one loop.
		if faketime != 0 {
			break
		}
	}
	if released > 0 && released < physPageSize {
		// 一次 s.scavenge 调用，要么回收0字节要么回收phySPageSize的整数倍。
		//
		// If this happens, it means that we may have attempted to release part
		// of a physical page, but the likely effect of that is that it released
		// the whole physical page, some of which may have still been in-use.
		// This could lead to memory corruption. Throw.
		throw("released less than one physical page of memory")
	}
	return
}

// Background scavenger.
//
// The background scavenger maintains the RSS of the application below
// the line described by the proportional scavenging statistics in
// the mheap struct.
//
// 会被以下3种机制唤醒：
// 1. sysmon
// 2. 每轮GC在完成上一轮GC清扫工作后会调用此函数， gcStart -> finishsweep_m()
// 3. scavenger 自己睡眠一段时间后，会在关联的 timer.f 中调用此函数。
func bgscavenge(c chan int) {
	scavenger.init()

	// 通知调用者，scavenge后台工作者已经初始化完成。
	c <- 1
	// 停靠后台scavenge工作协程，等待唤醒。
	//
	// 会被以下3种机制唤醒：
	// 1. sysmon
	// 2. 每轮GC在完成上一轮GC清扫工作后会调用此函数， gcStart -> finishsweep_m()
	// 3. scavenger 自己睡眠一段时间后，会在关联的 timer.f 中调用此函数。
	scavenger.park()

	for {
		// 回收了多少字节，此次回收工作花费的时间
		released, workTime := scavenger.run()
		if released == 0 {
			// 堆中没有可回收页面，不采用timer机制的停靠。
			// timer并不会唤醒这里的park。sysmon可以。
			scavenger.park()
			continue
		}
		atomic.Xadduintptr(&mheap_.pages.scav.released, released)
		// scavenger.sleep 会根据输入的工作时间(workTime)计算接下来需要sleep的时间。
		// 这里的parK由timer/sysmon唤醒。
		scavenger.sleep(workTime)
	}
}

// scavenge scavenges nbytes worth of free pages, starting with the
// highest address first. Successive calls continue from where it left
// off until the heap is exhausted. Call scavengeStartGen to bring it
// back to the top of the heap.
//
// Returns the amount of memory scavenged in bytes.
//
// scavenge always tries to scavenge nbytes worth of memory, and will
// only fail to do so if the heap is exhausted for now.
//
// 清理目标 nbytes，要么到达目标，要么堆没有可清理的页了。
// 返回scavenge的字节数。[对应的页由分配位0/scavenge位0变更到分配位为0，scavenge位1]
// 对这些地址区间执行sysUnused()调用。
// -----------
// 此函数会被后台scavenge工作者 和 同步scavenge工作者 调用
func (p *pageAlloc) scavenge(nbytes uintptr, shouldStop func() bool) uintptr {
	released := uintptr(0)
	for released < nbytes {
		// 通过 pageAlloc.scav.index.cunks 中保存的关于chunk中是否包含可scavenge页的元数据信息
		// 来找到可scavenge的chunk，就是返回值ci。
		// pageIdx chunk中索引最大的可清理页面在chunk中的相对偏移(相对于这个chunk的起始页)，调用者就是从这个页开始
		// 递减尝试清理。
		// 清理都是从高地址页到低地址页的方向进行的。
		ci, pageIdx := p.scav.index.find()
		if ci == 0 {
			break
		}
		systemstack(func() {
			released += p.scavengeOne(ci, pageIdx, nbytes-released)
		})
		// 如果scavenge执行花费cpu的时间超过限制，shouldStop()会返回true。
		// 停止继续scavenge。
		if shouldStop != nil && shouldStop() {
			break
		}
	}
	return released
}

// printScavTrace prints a scavenge trace line to standard error.
//
// released should be the amount of memory released since the last time this
// was called, and forced indicates whether the scavenge was forced by the
// application.
//
// scavenger.lock must be held.
//
// 在 sweepone 函数中调用
// 即在每次清扫结束时调用。
func printScavTrace(released uintptr, forced bool) {
	assertLockHeld(&scavenger.lock)

	printlock()
	// scav:本轮GC结束时，被后台scavege回收的页面的总大小，这些页
	// 由分配位0，scavenge位0 -> 标记位0，scavenge位1。并将这些页的状态由
	// ready变化到prepared状态。
	//
	// heapReleased:页堆中所有分配位为0，scavenge位为1的页(这些页都是prepared状态)的总
	// 字节大小。
	//
	// heapRtained:页堆中所有分配位为1，scavenge位为0的页+所有分配位为0，scavenge位为0的页
	// 总字节大小。
	print("scav ",
		released>>10, " KiB work, ",
		gcController.heapReleased.load()>>10, " KiB total, ",
		(gcController.heapInUse.load()*100)/heapRetained(), "% util",
	)
	// debug.FreeOSMemory 函数调用回收页时forced为true。
	// 清扫终止阶段时此值为false。
	if forced {
		print(" (forced)")
	} else if scavenger.printControllerReset {
		// controllerFailed indicates that the scavenger's scheduling
		// controller failed.
		print(" [controller reset]")
		scavenger.printControllerReset = false
	}
	println()
	printunlock()
}

// scavengeOne walks over the chunk at chunk index ci and searches for
// a contiguous run of pages to scavenge. It will try to scavenge
// at most max bytes at once, but may scavenge more to avoid
// breaking huge pages. Once it scavenges some memory it returns
// how much it scavenged in bytes.
//
// searchIdx is the page index to start searching from in ci.
//
// Returns the number of bytes scavenged.
//
// Must run on the systemstack because it acquires p.mheapLock.
//
// 清理 ci对应的chunk（512页*8192）中的所有可清理页,一次只能清理整数个页面同时受限于max参数，
// 在上锁的情况下，清理先将查找到符合要求的连续页，然后标记为已分配，防止被申请使用了，然后调用 sysUnused
// 告知操作系统这些页可以回收了。然后会取消这些页的已分配标记并被 scavenge 了。
// 同时可能还会会更新chunks中的位图，如果整个chun都清理了。
//
// sysUnused transitions a memory region from Ready to Prepared
//
// 参数：
// ci 要清理的chunk的索引，通过此索引在 pageAlloc.chunks 中可以找到对应的chunk的位图信息。
// searchIdx ci中的页面偏移量，也就是说从ci这个chunk的serachIdx索引的页开始查找可清理页面。
// max 此次最多允许清理max bytes
//
// 返回实际清理的bytes，因为是整页清理实际清理的字节数为 n*pageSize(n*8192)。还有就是如果清理的页包含在
// linux 的hugepage中，会将清理整个hugepage。这都导致实际清理(返回值)的字节数大于参数max指定的字节数。
//
// 影响：
// 清理是将页由alloc位=0，scavenged位=0，变更到alloc位=0，scavenged位=1。
// 被清理的页的地址区间由ready状态到prepared状态。
//
//go:systemstack
func (p *pageAlloc) scavengeOne(ci chunkIdx, searchIdx uint, max uintptr) uintptr {
	// Calculate the maximum number of pages to scavenge.
	//
	// This should be alignUp(max, pageSize) / pageSize but max can and will
	// be ^uintptr(0), so we need to be very careful not to overflow here.
	// Rather than use alignUp, calculate the number of pages rounded down
	// first, then add back one if necessary.
	maxPages := max / pageSize
	if max%pageSize != 0 {
		maxPages++
	}

	// Calculate the minimum number of pages we can scavenge.
	//
	// Because we can only scavenge whole physical pages, we must
	// ensure that we scavenge at least minPages each time, aligned
	// to minPages*pageSize.
	minPages := physPageSize / pageSize
	if minPages < 1 {
		minPages = 1
	}

	lock(p.mheapLock)
	if p.summary[len(p.summary)-1][ci].max() >= uint(minPages) {
		// 在 p.summary[len(p.summary)-1][ci] 这个chunk中存在>=minPages张连续的未分配页。
		// 其实未分配页不一定是可scavenged，只有同时满足未清扫未scavenged得才是可scavengen的页。
		//
		// We only bother looking for a candidate if there at least
		// minPages free pages at all.
		//
		// base 是符合条件连续页的起始页偏移量(在此chunk的512中的偏移量)。
		// npages 是从base开始存在npages中可scavenged的页。
		// 这段地址区间可以表示为 [base*pageSize+此chunk的起始地址，base*pageSize+此chunk的起始地址+npages*pageSize)
		base, npages := p.chunkOf(ci).findScavengeCandidate(searchIdx, minPages, maxPages)

		// If we found something, scavenge it and return!
		if npages != 0 {
			// 发现了可scavengen的页。
			//
			// Compute the full address for the start of the range.
			//
			// 起始页的绝对地址。
			addr := chunkBase(ci) + uintptr(base)*pageSize

			// Mark the range we're about to scavenge as allocated, because
			// we don't want any allocating goroutines to grab it while
			// the scavenging is in progress.
			if scav := p.allocRange(addr, uintptr(npages)); scav != 0 {
				// npages 对应的scavenge位都应该是0，所以scav应返回0才对。
				throw("double scavenge")
			}
			// 上面的p.allocRange调用之后，npages对应的所有分配位都被置为了1，且在pageAlloc.summary中各层的
			// sum信息页得到了更新。

			// With that done, it's safe to unlock.
			unlock(p.mheapLock)

			if !p.test {
				pageTraceScav(getg().m.p.ptr(), 0, addr, uintptr(npages))

				// Only perform the actual scavenging if we're not in a test.
				// It's dangerous to do so otherwise.
				//
				// 将npage的地址区间从ready状态到prepared状态。
				sysUnused(unsafe.Pointer(addr), uintptr(npages)*pageSize)

				// Update global accounting only when not in test, otherwise
				// the runtime's accounting will be wrong.
				nbytes := int64(npages) * pageSize
				//
				//
				gcController.heapReleased.add(nbytes)
				gcController.heapFree.add(-nbytes)

				stats := memstats.heapStats.acquire()
				atomic.Xaddint64(&stats.committed, -nbytes)
				atomic.Xaddint64(&stats.released, nbytes)
				memstats.heapStats.release()
			}

			// Relock the heap, because now we need to make these pages
			// available allocation. Free them back to the page allocator.
			lock(p.mheapLock)
			// 将npages对应的分配位都置为0，并更新相应的sum信息。
			p.free(addr, uintptr(npages), true)

			// Mark the range as scavenged.
			// 将npages对应的所有scavenged都置为1。
			p.chunkOf(ci).scavenged.setRange(base, npages)
			unlock(p.mheapLock)

			return uintptr(npages) * pageSize
		}
	}
	// Mark this chunk as having no free pages.
	//
	// ci索引对应的chuk已经没有可清扫页了。这里的情况是ci所有页都被分配了。
	// 所以要将其在 pageAlloc.scav.index中的位置为0。
	// 当此chuk包含的页都已经被分配了会出现这种情况，即它们的分配为都是1，scavenged位是0。
	p.scav.index.clear(ci)
	unlock(p.mheapLock)

	return 0
}

// fillAligned returns x but with all zeroes in m-aligned groups of m bits set to 1 if any bit in the group is non-zero.
//
// For example, fillAligned(0x0100a3, 8) == 0xff00ff.
//
// Note that if m == 1, this is a no-op.
//
// m must be a power of 2 <= maxPagesPerPhysPage.
//
// 将x的位(64位bit)分为m组，因为m是2^n的结果，所以从高位分和从低位分结果是一样的。
// 如果任何一组的位不全是0，则这组所有的位都置为1。然后在联接成64位返回结果。
//
// 比如 x=0b001100，m=2。则结果还是x。只要分组之后只有第2位和第3位所在组包含1。而它俩本轮都是1了，其它组都是0所以原样不动。
func fillAligned(x uint64, m uint) uint64 {
	apply := func(x uint64, c uint64) uint64 {
		// The technique used it here is derived from
		// https://graphics.stanford.edu/~seander/bithacks.html#ZeroInWord
		// and extended for more than just bytes (like nibbles
		// and uint16s) by using an appropriate constant.
		//
		// To summarize the technique, quoting from that page:
		// "[It] works by first zeroing the high bits of the [8]
		// bytes in the word. Subsequently, it adds a number that
		// will result in an overflow to the high bit of a byte if
		// any of the low bits were initially set. Next the high
		// bits of the original word are ORed with these values;
		// thus, the high bit of a byte is set iff any bit in the
		// byte was set. Finally, we determine if any of these high
		// bits are zero by ORing with ones everywhere except the
		// high bits and inverting the result."
		return ^((((x & c) + c) | x) | c)
	}
	// Transform x to contain a 1 bit at the top of each m-aligned
	// group of m zero bits.
	switch m {
	case 1:
		return x
	case 2:
		x = apply(x, 0x5555555555555555)
	case 4:
		x = apply(x, 0x7777777777777777)
	case 8:
		x = apply(x, 0x7f7f7f7f7f7f7f7f)
	case 16:
		x = apply(x, 0x7fff7fff7fff7fff)
	case 32:
		x = apply(x, 0x7fffffff7fffffff)
	case 64: // == maxPagesPerPhysPage
		x = apply(x, 0x7fffffffffffffff)
	default:
		throw("bad m value")
	}
	// Now, the top bit of each m-aligned group in x is set
	// that group was all zero in the original x.

	// From each group of m bits subtract 1.
	// Because we know only the top bits of each
	// m-aligned group are set, we know this will
	// set each group to have all the bits set except
	// the top bit, so just OR with the original
	// result to set all the bits.
	return ^((x - (x >> (m - 1))) | x)
}

// findScavengeCandidate returns a start index and a size for this pallocData
// segment which represents a contiguous region of free and unscavenged memory.
//
// searchIdx indicates the page index within this chunk to start the search, but
// note that findScavengeCandidate searches backwards through the pallocData. As a
// a result, it will return the highest scavenge candidate in address order.
//
// min indicates a hard minimum size and alignment for runs of pages. That is,
// findScavengeCandidate will not return a region smaller than min pages in size,
// or that is min pages or greater in size but not aligned to min. min must be
// a non-zero power of 2 <= maxPagesPerPhysPage.
//
// max is a hint for how big of a region is desired. If max >= pallocChunkPages, then
// findScavengeCandidate effectively returns entire free and unscavenged regions.
// If max < pallocChunkPages, it may truncate the returned region such that size is
// max. However, findScavengeCandidate may still return a larger region if, for
// example, it chooses to preserve huge pages, or if max is not aligned to min (it
// will round up). That is, even if max is small, the returned size is not guaranteed
// to be equal to max. max is allowed to be less than min, in which case it is as if
// max == min.
//
// 作用: 此函数在m(pallocData，包括chunk的分配位图和scavenged位图)，中找到符合参数条件的连续未分配
// 且未scavenged的页，返回起始页在chunk包含的512页中的偏移量（即起始页）和从这个起始页开始有多少个
// 页面可供scavengen。
//
// 查找时是从 searchIdx 索引的页到0索引的页的方向查找[m关联chunk的512页的索引]。pallocData 底层数组的高索引
// 元素像低索引元素开始查找。
// 比如 pallocData 索引0对应的uint64为 0x1100110000110011 索引1对应的为0x1122334455667788，searchIdx
// 为15。则会从索引1对应的uint64元素的最高位开始搜索，往索引1对应的uint64元素的最低位方向搜索，因为主机是小端序。
// 如果从页地址大小来看，其从高地址页往低地地址页开始搜索。
//
// 参数:
// min需满足为2^n且小于等于64。
// max需对齐到min，对齐后的max记为alignedMax。满足: alignedMax>=min。
//
// searchIdx 从searchIdx对应在m中对应的页往低地址搜索，即从searchIdx->0搜索(m对应的512页的索引)。
// 一旦找到连续n>=min张满足未分配未scavenged的页便停止。
// min 此次最小需找到min张连续可scavengen的页。
// max 此次最多返回aligenedMax张连续可scavengen的页。
// 不过max除了需对齐到min，还需考虑linux平台下的pysHuge页的情况。此时不会考虑alignMax的值。
//
// 第一个返回值start
// 在m中对应的chunk的512页的偏移量，从这个页面开始scavengen。
// 如果n>max，则n=max。如果这n(没有设置成max的n)页中包含了一个完整的 physHugePageSize 。此时便不会考虑max，
// start=这个pysHuge页的起始页在512页中的偏移量，run为n-pysHugeSize/physize。
// 第二个返回值run
// 表示从start偏移量开始找到了run张符合调用的可scavengen的页。
//
func (m *pallocData) findScavengeCandidate(searchIdx uint, min, max uintptr) (uint, uint) {
	if min&(min-1) != 0 || min == 0 {
		print("runtime: min = ", min, "\n")
		throw("min must be a non-zero power of 2")
	} else if min > maxPagesPerPhysPage {
		print("runtime: min = ", min, "\n")
		throw("min too large")
	}
	// max may not be min-aligned, so we might accidentally truncate to
	// a max value which causes us to return a non-min-aligned value.
	// To prevent this, align max up to a multiple of min (which is always
	// a power of 2). This also prevents max from ever being less than
	// min, unless it's zero, so handle that explicitly.
	if max == 0 {
		max = min
	} else {
		max = alignUp(max, min)
	}

	i := int(searchIdx / 64)
	// Start by quickly skipping over blocks of non-free or scavenged pages.
	for ; i >= 0; i-- {
		// 从searchIdx所在的两个pageBits的元素(uint64类型)，一个清扫bitmap和一个scavenged bitmap。
		// 让它们做按位与操作结果记作R。
		// 开始往后搜索，一旦发现某个R中有连续min个0(见下面的描述)，便停止搜索。
		//
		// R=110011, m=2。此时R满足。
		// R=111001, m=2。此时R不满足。将R按m从低位开始分组 11 10 01 没有一组是两个0，所以R不符合。
		// 1s are scavenged OR non-free => 0s are unscavenged AND free
		x := fillAligned(m.scavenged[i]|m.pallocBits[i], uint(min))
		if x != ^uint64(0) {
			break
		}
	}
	if i < 0 {
		// Failed to find any free/unscavenged pages.
		return 0, 0
	}
	// We have something in the 64-bit chunk at i, but it could
	// extend further. Loop until we find the extent of it.

	// 1s are scavenged OR non-free => 0s are unscavenged AND free
	x := fillAligned(m.scavenged[i]|m.pallocBits[i], uint(min))
	// i=1
	// 比如 x= 0b00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000011,m=2
	// 取反 ^x=0b11111111 11111111 11111111 11111111 11111111 11111111 11111111 11111100
	// 则z1=0
	z1 := uint(sys.LeadingZeros64(^x))
	// run,end=0,128
	run, end := uint(0), uint(i)*64+(64-z1)
	if x<<z1 != 0 {
		// 从高位到低位，x第一组连续m个零之后低位不是连续的0。
		// 比如上面的x第一组连续m个0是第63和第62位，但其低位方法不全是0(第1位和第0位是1)。
		//
		// After shifting out z1 bits, we still have 1s,
		// so the run ends inside this word.
		//
		// run=64-2=62，x前导有62个0。
		// start=128-62=64+2，也就是从x的第三位开始搜索。
		run = uint(sys.LeadingZeros64(x << z1))
	} else {
		// 从高位到低位，如果R(i)即x中第一组m个0的低位方向都是0，则继续查看R(i-1)的高位看有没有连续的0。
		// 目的是为了一次尽可能多的scavengen页。
		//
		// After shifting out z1 bits, we have no more 1s.
		// This means the run extends to the bottom of the
		// word so it may extend into further words.
		run = 64 - z1
		for j := i - 1; j >= 0; j-- {
			x := fillAligned(m.scavenged[j]|m.pallocBits[j], uint(min))
			run += uint(sys.LeadingZeros64(x))
			if x != 0 {
				// The run stopped in this word.
				break
			}
		}
		// 这里的R(x)指的是 fillAligned(m.scavenged[j]|m.pallocBits[j], uint(min))。
		//
		// 上面循环做的是：从R(i-1)最高位开始往低位查看(可以跨越uint64元素)，如果有连续n个0。则run+64-z1+n。
		//
		// 假如：如果R(i-1)最高位往低位看有连续n个0，则run=64-z1+n，这里假设R(i-1)不全是0。
		// 不过如果R(i-1)所有位都是0，会继续查看R(i-2)。直到遇到R(i-n)不全是0。
	}

	// Split the run we found if it's larger than max but hold on to
	// our original length, since we may need it later.
	//
	// 因为采用有贪婪方式获取连续可scavengen页。所以run可能大于max。
	size := run
	if size > uint(max) {
		size = uint(max)
	}
	// start 就是m(pallocData类型)中可scavengen(且符合前面的计算要求)的起始页在m关联的chunk中的512页中的偏移量。
	// size 就是从start页开始清扫size个页面。
	start := end - size

	// Each huge page is guaranteed to fit in a single palloc chunk.
	//
	// TODO(mknyszek): Support larger huge page sizes.
	// TODO(mknyszek): Consider taking pages-per-huge-page as a parameter
	// so we can write tests for this.
	//
	// 2MB linxu amd64 > 8192 && 2MB > 4906
	if physHugePageSize > pageSize && physHugePageSize > physPageSize {
		// linux amd64 为true。
		//
		// We have huge pages, so let's ensure we don't break one by scavenging
		// over a huge page boundary. If the range [start, start+size) overlaps with
		// a free-and-unscavenged huge page, we want to grow the region we scavenge
		// to include that huge page.

		// Compute the huge page boundary above our candidate.
		pagesPerHugePage := uintptr(physHugePageSize / pageSize)
		// pagesPerHugePage = 2^21/2^13=2^8=256个。
		hugePageAbove := uint(alignUp(uintptr(start), pagesPerHugePage))

		// If that boundary is within our current candidate, then we may be breaking
		// a huge page.
		if hugePageAbove <= end {
			// Compute the huge page boundary below our candidate.
			hugePageBelow := uint(alignDown(uintptr(start), pagesPerHugePage))

			if hugePageBelow >= end-run {
				// We're in danger of breaking apart a huge page since start+size crosses
				// a huge page boundary and rounding down start to the nearest huge
				// page boundary is included in the full run we found. Include the entire
				// huge page in the bound by rounding down to the huge page size.
				//
				// size = end-start + (start-hugePageBelow)=end-hugePageBelow。
				// size = end-hugePageBelow。
				size = size + (start - hugePageBelow)
				start = hugePageBelow
			}
		}
	}
	return start, size
}

// scavengeIndex is a structure for efficiently managing which pageAlloc chunks have
// memory available to scavenge.
type scavengeIndex struct {
	// chunks is a bitmap representing the entire address space. Each bit represents
	// a single chunk(4MB), and a 1 value indicates the presence of pages available for
	// scavenging. Updates to the bitmap are serialized by the pageAlloc lock.
	//
	// The underlying storage of chunks is platform dependent and may not even be
	// totally mapped read/write. min and max reflect the extent that is safe to access.
	// min is inclusive, max is exclusive.
	// [min,max), [ scavengeIndex.chunks[min], scavengeIndex.chunks[max])
	//
	// searchAddr is the maximum address (in the offset address space, so we have a linear
	// view of the address space; see mranges.go:offAddr) containing memory available to
	// scavenge. It is a hint to the find operation to avoid O(n^2) behavior in repeated lookups.
	//
	// searchAddr is always inclusive and should be the base address of the highest runtime
	// page available for scavenging.
	//
	// searchAddr is managed by both find and mark.
	//
	// Normally, find monotonically decreases searchAddr as it finds no more free pages to
	// scavenge. However, mark, when marking a new chunk at an index greater than the current
	// searchAddr, sets searchAddr to the *negative* index into chunks of that page. The trick here
	// is that concurrent calls to find will fail to monotonically decrease searchAddr, and so they
	// won't barge over new memory becoming available to scavenge. Furthermore, this ensures
	// that some future caller of find *must* observe the new high index. That caller
	// (or any other racing with it), then makes searchAddr positive before continuing, bringing
	// us back to our monotonically decreasing steady-state.
	//
	// A pageAlloc lock serializes updates between min, max, and searchAddr, so abs(searchAddr)
	// is always guaranteed to be >= min and < max (converted to heap addresses).
	//
	// TODO(mknyszek): Ideally we would use something bigger than a uint8 for faster
	// iteration like uint32, but we lack the bit twiddling intrinsics. We'd need to either
	// copy them from math/bits or fix the fact that we can't import math/bits' code from
	// the runtime due to compiler instrumentation.
	//
	// 相对于 arenaBaseOffset 的偏移量。
	//
	// 进行 scavenge 的指示器，比(此值+pageSize-1)大的地址所在的页
	// 肯定不包含可scavenged的页。此值为变量的虚拟地址- arenaBaseOffset 所得到的值。
	// 即这些页不满足分配位为0，scavenged位为0的要求。
	//
	// 可能为负,表示已经被 mark 方法标记,find方法观察到为负时会修正。
	//
	// 在 mark 方法中变大(可能)。
	// 在 find 方法中变小(可能)。
	// 只会在这来个方法中被修改。
	searchAddr atomicOffAddr
	//
	// 此切片中的每一个位代表一个chunk，如果此位为1表示此chunk包含可scavenge的页。
	// 1 value indicates the presence of pages available for scavenging.
	//
	// 在 clear 方法中可能会将某个位置为0，如果其对应的chunk中所有的页都被分配了。
	// 在 mark 方法中可能某些位置为1，表示对应的chunk中至少包含一个可scavenged的页面。
	chunks     []atomic.Uint8
	// minHeapIdx 表示[min,max)区间中的最小有效值，有效即 chunks[minHeapIdx]
	// 不等于0。
	minHeapIdx atomic.Int32
	// [min,max)表示chunks中可以安全使用的索引。
	min, max   atomic.Int32
}

// find returns the highest chunk index that may contain pages available to scavenge.
// It also returns an offset to start searching in the highest chunk.
//
// 返回值：
// chunk 的编号
// uint 可以清理页面在chunk中的相对偏移(相对于这个chunk的起始页)，调用者就是从这个页开始
// 递减尝试清理。
// 返回的可能是chunk
//
// 这个函数复制减少 searchAddr 的值，如果减失败了
// 就按最大可能的结果返回，所以只会损失点性能但不会丢失要清扫的页。
func (s *scavengeIndex) find() (chunkIdx, uint) {
	searchAddr, marked := s.searchAddr.Load()
	// marked为true表示s.searchAddr被s.mark函数更新了。

	if searchAddr == minOffAddr.addr() {
		// 堆中没有任何可scavenged的页面存在。
		// We got a cleared search addr.
		return 0, 0
	}

	// Starting from searchAddr's chunk, and moving down to minHeapIdx,
	// iterate until we find a chunk with pages to scavenge.
	min := s.minHeapIdx.Load()
	searchChunk := chunkIndex(uintptr(searchAddr))

	// 一个s.chunks[i]对应8个chunk
	start := int32(searchChunk / 8)
	for i := start; i >= min; i-- {
		// Skip over irrelevant address space.
		chunks := s.chunks[i].Load()
		if chunks == 0 {
			continue
		}
		// Note that we can't have 8 leading zeroes here because
		// we necessarily skipped that case. So, what's left is
		// an index. If there are no zeroes, we want the 7th
		// index, if 1 zero, the 6th, and so on.
		n := 7 - sys.LeadingZeros8(chunks)

		ci := chunkIdx(uint(i)*8 + uint(n))
		// ci倒数是s.chunks[i](包含8个chunk)中第一个包含可清理页的chunk的索引。
		if searchChunk == ci {
			// 包含可清扫的页是searchAddr所属的chunk
			return ci, chunkPageIndex(uintptr(searchAddr))
		}
		// Try to reduce searchAddr to newSearchAddr.
		//
		// newSearchAddr是以这个ci索引的chunk(一个chunk包含512个页面)中最后一页的起始地址。
		newSearchAddr := chunkBase(ci) + pallocChunkBytes - pageSize
		if marked {
			// Attempt to be the first one to decrease the searchAddr
			// after an increase. If we fail, that means there was another
			// increase, or somebody else got to it before us. Either way,
			// it doesn't matter. We may lose some performance having an
			// incorrect search address, but it's far more important that
			// we don't miss updates.
			//
			// 可能会失败，
			s.searchAddr.StoreUnmark(searchAddr, newSearchAddr)
		} else {
			// Decrease searchAddr.
			// 可能也会失败，其执行find的存储了一个更小的值。
			s.searchAddr.StoreMin(newSearchAddr)
		}
		return ci, pallocChunkPages - 1
	}
	// Clear searchAddr, because we've exhausted the heap.
	// 尝试将偏移量清零。
	s.searchAddr.Clear()
	return 0, 0
}

// mark sets the inclusive range of chunks between indices start and end as
// containing pages available to scavenge.
//
// Must be serialized with other mark, markRange, and clear calls.
//
// [base,limit)表示的地址区间正好包含n个page(8192)。
//
// 将个地址区间所跨的的chunks，记为[start,end]。将s.chuks中对应的元素中对应的这些chuks的
// 位置为1。标志着对应的chunk中包含可scavenged的页。
// 并按条件更新 scavengeIndex.searchAddr。
func (s *scavengeIndex) mark(base, limit uintptr) {
	start, end := chunkIndex(base), chunkIndex(limit-pageSize)
	if start == end {
		// Within a chunk.
		mask := uint8(1 << (start % 8))
		// 将start chunk在s.chunks中对应的位置为1。
		s.chunks[start/8].Or(mask)
	} else if start/8 == end/8 {
		// [start,end]区间包含所有的chunk都在s.chunks中的一个元素内(uint8类型)。
		// Within the same byte in the index.
		//
		// 0b10000110
		// start=1,end=2
		// 1<<2=4, 4-1=3
		// 11<<1
		// 0b110or0b10000110 = 0b10000110
		mask := uint8(uint16(1<<(end-start+1))-1) << (start % 8)
		// [start-end]区间对应的chuks，s.chunks中对应的位置为1。
		s.chunks[start/8].Or(mask)
	} else {
		// [start,end]跨越多个s.chuks中的元素。
		// Crosses multiple bytes in the index.
		startAligned := chunkIdx(alignUp(uintptr(start), 8))
		endAligned := chunkIdx(alignDown(uintptr(end), 8))

		// Do the end of the first byte first.
		if width := startAligned - start; width > 0 {
			mask := uint8(uint16(1<<width)-1) << (start % 8)
			s.chunks[start/8].Or(mask)
		}
		// Do the middle aligned sections that take up a whole
		// byte.
		for ci := startAligned; ci < endAligned; ci += 8 {
			s.chunks[ci/8].Store(^uint8(0))
		}
		// Do the end of the last byte.
		//
		// This width check doesn't match the one above
		// for start because aligning down into the endAligned
		// block means we always have at least one chunk in this
		// block (note that end is *inclusive*). This also means
		// that if end == endAligned+n, then what we really want
		// is to fill n+1 chunks, i.e. width n+1. By induction,
		// this is true for all n.
		if width := end - endAligned + 1; width > 0 {
			mask := uint8(uint16(1<<width) - 1)
			s.chunks[end/8].Or(mask)
		}
	}
	newSearchAddr := limit - pageSize
	searchAddr, _ := s.searchAddr.Load()
	// N.B. Because mark is serialized, it's not necessary to do a
	// full CAS here. mark only ever increases searchAddr, while
	// find only ever decreases it. Since we only ever race with
	// decreases, even if the value we loaded is stale, the actual
	// value will never be larger.
	//
	// 如果新增的可清扫页中最大页地址(地址最高页的起始地址)比s.searchAddr大的话
	// 就将s.searchAddr更新为此最大页地址。
	if (offAddr{searchAddr}).lessThan(offAddr{newSearchAddr}) {
		s.searchAddr.StoreMarked(newSearchAddr)
	}
}

// clear sets the chunk at index ci as not containing pages available to scavenge.
//
// Must be serialized with other mark, markRange, and clear calls.
func (s *scavengeIndex) clear(ci chunkIdx) {
	s.chunks[ci/8].And(^uint8(1 << (ci % 8)))
}

type piController struct {
	kp float64 // Proportional constant.
	// 3.2e6
	ti float64 // Integral time constant.
	// 1e9
	tt float64 // Reset time.

	// 0.001 和 1000
	min, max float64 // Output boundaries.

	// PI controller state.

	errIntegral float64 // Integral of the error from t=0 to now.

	// Error flags.
	errOverflow   bool // Set if errIntegral ever overflowed.
	inputOverflow bool // Set if an operation with the input overflowed.
}

// next provides a new sample to the controller.
//
// input is the sample, setpoint is the desired point, and period is how much
// time (in whatever unit makes the most sense) has passed since the last sample.
//
// Returns a new value for the variable it's controlling, and whether the operation
// completed successfully. One reason this might fail is if error has been growing
// in an unbounded manner, to the point of overflow.
//
// In the specific case of an error overflow occurs, the errOverflow field will be
// set and the rest of the controller's internal state will be fully reset.
func (c *piController) next(input, setpoint, period float64) (float64, bool) {
	// Compute the raw output value.
	// 0.3374
	prop := c.kp * (setpoint - input)
	rawOutput := prop + c.errIntegral

	// Clamp rawOutput into output.
	output := rawOutput
	if isInf(output) || isNaN(output) {
		// The input had a large enough magnitude that either it was already
		// overflowed, or some operation with it overflowed.
		// Set a flag and reset. That's the safest thing to do.
		c.reset()
		c.inputOverflow = true
		return c.min, false
	}
	if output < c.min {
		output = c.min
	} else if output > c.max {
		output = c.max
	}

	// Update the controller's state.
	if c.ti != 0 && c.tt != 0 {
		// ti: 3.2e6
		// tt: 1e9
		// c.kp: 0.3374
		c.errIntegral += (c.kp*period/c.ti)*(setpoint-input) + (period/c.tt)*(output-rawOutput)
		if isInf(c.errIntegral) || isNaN(c.errIntegral) {
			// So much error has accumulated that we managed to overflow.
			// The assumptions around the controller have likely broken down.
			// Set a flag and reset. That's the safest thing to do.
			c.reset()
			c.errOverflow = true
			return c.min, false
		}
	}
	return output, true
}

// reset resets the controller state, except for controller error flags.
func (c *piController) reset() {
	c.errIntegral = 0
}
