// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"internal/goexperiment"
	"runtime/internal/atomic"
	_ "unsafe" // for go:linkname
)

// go119MemoryLimitSupport is a feature flag for a number of changes
// related to the memory limit feature (#48409). Disabling this flag
// disables those features, as well as the memory limit mechanism,
// which becomes a no-op.
const go119MemoryLimitSupport = true

const (
	// gcGoalUtilization is the goal CPU utilization for
	// marking as a fraction of GOMAXPROCS.
	//
	// Increasing the goal utilization will shorten GC cycles as the GC
	// has more resources behind it, lessening costs from the write barrier,
	// but comes at the cost of increasing mutator latency.
	gcGoalUtilization = gcBackgroundUtilization

	// gcBackgroundUtilization is the fixed CPU utilization for background
	// marking. It must be <= gcGoalUtilization. The difference between
	// gcGoalUtilization and gcBackgroundUtilization will be made up by
	// mark assists. The scheduler will aim to use within 50% of this
	// goal.
	//
	// As a general rule, there's little reason to set gcBackgroundUtilization
	// < gcGoalUtilization. One reason might be in mostly idle applications,
	// where goroutines are unlikely to assist at all, so the actual
	// utilization will be lower than the goal. But this is moot point
	// because the idle mark workers already soak up idle CPU resources.
	// These two values are still kept separate however because they are
	// distinct conceptually, and in previous iterations of the pacer the
	// distinction was more important.
	gcBackgroundUtilization = 0.25

	// gcCreditSlack is the amount of scan work credit that can
	// accumulate locally before updating gcController.heapScanWork and,
	// optionally, gcController.bgScanCredit. Lower values give a more
	// accurate assist ratio and make it more likely that assists will
	// successfully steal background credit. Higher values reduce memory
	// contention.
	gcCreditSlack = 2000

	// gcAssistTimeSlack is the nanoseconds of mutator assist time that
	// can accumulate on a P before updating gcController.assistTime.
	gcAssistTimeSlack = 5000

	// gcOverAssistWork determines how many extra units of scan work a GC
	// assist does when an assist happens. This amortizes the cost of an
	// assist by pre-paying for this many bytes of future allocations.
	gcOverAssistWork = 64 << 10

	// defaultHeapMinimum is the value of heapMinimum for GOGC==100.
	defaultHeapMinimum = (goexperiment.HeapMinimum512KiBInt)*(512<<10) +
		(1-goexperiment.HeapMinimum512KiBInt)*(4<<20)

	// maxStackScanSlack is the bytes of stack space allocated or freed
	// that can accumulate on a P before updating gcController.stackSize.
	maxStackScanSlack = 8 << 10

	// memoryLimitHeapGoalHeadroom is the amount of headroom the pacer gives to
	// the heap goal when operating in the memory-limited regime. That is,
	// it'll reduce the heap goal by this many extra bytes off of the base
	// calculation.
	memoryLimitHeapGoalHeadroom = 1 << 20
)

// gcController implements the GC pacing controller that determines
// when to trigger concurrent garbage collection and how much marking
// work to do in mutator assists and background marking.
//
// It calculates the ratio between the allocation rate (in terms of CPU
// time) and the GC scan throughput to determine the heap size at which to
// trigger a GC cycle such that no GC assists are required to finish on time.
// This algorithm thus optimizes GC CPU utilization to the dedicated background
// mark utilization of 25% of GOMAXPROCS by minimizing GC assists.
// GOMAXPROCS. The high-level design of this algorithm is documented
// at https://github.com/golang/proposal/blob/master/design/44167-gc-pacer-redesign.md.
// See https://golang.org/s/go15gcpacing for additional historical context.
var gcController gcControllerState

type gcControllerState struct {
	// Initialized from GOGC. GOGC=off means no GC.
	gcPercent atomic.Int32

	// memoryLimit is the soft memory limit in bytes.
	//
	// Initialized from GOMEMLIMIT. GOMEMLIMIT=off is equivalent to MaxInt64
	// which means no soft memory limit in practice.
	//
	// This is an int64 instead of a uint64 to more easily maintain parity with
	// the SetMemoryLimit API, which sets a maximum at MaxInt64. This value
	// should never be negative.
	memoryLimit atomic.Int64

	// heapMinimum is the minimum heap size at which to trigger GC.
	// For small heaps, this overrides the usual GOGC*live set rule.
	//
	// When there is a very small live set but a lot of allocation, simply
	// collecting when the heap reaches GOGC*live results in many GC
	// cycles and high total per-GC overhead.
	//
	// This minimum amortizes this
	// per-GC overhead while keeping the heap reasonably small.
	//
	// During initialization this is set to 4MB*GOGC/100. In the case of
	// GOGC==0, this will set heapMinimum to 0, resulting in constant
	// collection even when the heap size is small, which is useful for
	// debugging.
	//
	// heapMinimum 初始值为4MB*GOGC/100，为了缓解当存活的堆很小，但分配比较频繁的情况。
	heapMinimum uint64

	// runway is the amount of runway in heap bytes allocated by the
	// application that we want to give the GC once it starts.
	//
	// This is computed from consMark during mark termination.
	runway atomic.Uint64

	// consMark is the estimated per-CPU consMark ratio for the application.
	//
	// It represents the ratio between the application's allocation
	// rate, as bytes allocated per CPU-time, and the GC's scan rate,
	// as bytes scanned per CPU-time.
	// The units of this ratio are (B / cpu-ns) / (B / cpu-ns).
	//
	// At a high level, this value is computed as the bytes of memory
	// allocated (cons) per unit of scan work completed (mark) in a GC
	// cycle, divided by the CPU time spent on each activity.
	//
	// Updated at the end of each GC cycle, in endCycle.
	//
	// 测试了下，应该总是越小越好：1.299068e-002
	//
	// GC标记结束时计算，结果为最近两次(此轮GC和上轮GC)的 lastConsMark 的平均值。
	//
	// 分配速率与标记速率的比值
	consMark float64

	// lastConsMark is the computed cons/mark value for the previous GC
	// cycle. Note that this is *not* the last value of cons/mark, but the
	// actual computed value. See endCycle for details.
	//
	// raw currentConsMark 每轮GC标记结束时更新，根据此轮标记情况计算而出。
	// 分配速率与标记速率的比值
	lastConsMark float64

	// gcPercentHeapGoal is the goal heapLive for when next GC ends derived
	// from gcPercent.
	//
	// Set to ^uint64(0) if gcPercent is disabled.
	//
	// 最小值为: 4MB*GOGC/100 (gcControllerState.heapMinimum)。
	// 在每次GC标记终止阶段更新：
	// gcPercentHeapGoal = c.heapMarked + (gcControllerState.heapMarked+ gcControllerState.lastStackScan+gcControllerState.globalsScan)*uint64(gcPercent)/100
	// 是下一次heap Trigger 触发添加的判断依据。
	//
	// 初始值在 gcControllerState.init , isSweepDone = true 以此为参数来调用 commit 函数，在其中更新。
	// 因为初始时，其它几项数据都是0，所以结果为 c.globalsScan.Load()*uint64(gcPercent)/100
	// 而 c.globalsScan 的值为所有模块的 data 和 bass 区间大小的和。
	//
	// ----------------------------------------------------
	// 目标堆大小，达到这个目标时就会触发GC。
	//
	// 第一次GC触发条件, heapLive >= min(c.globalsScan.Load()*uint64(gcPercent)/100,gcControllerState.heapMinimum)。
	// 之后GC触发条件， heapLive >= min(c.heapMarked + (gcControllerState.heapMarked+ gcControllerState.lastStackScan+gcControllerState.globalsScan)*uint64(gcPercent)/100,
	// gcControllerState.heapMinimum)
	//
	// 只在 gcMarkTermination -> gcControllerCommit(此时已经处于_GCoff状态) 调用中更新。
	gcPercentHeapGoal atomic.Uint64

	// sweepDistMinTrigger is the minimum trigger to ensure a minimum
	// sweep distance.
	//
	// This bound is also special because it applies to both the trigger
	// *and* the goal (all other trigger bounds must be based *on* the goal).
	//
	// It is computed ahead of time, at commit time. The theory is that,
	// absent a sudden change to a parameter like gcPercent, the trigger
	// will be chosen to always give the sweeper enough headroom. However,
	// such a change might dramatically and suddenly move up the trigger,
	// in which case we need to ensure the sweeper still has enough headroom.
	//
	// 在函数 HeapGoalInternal 中读取，在 commit 函数中修改。
	// gcControllerState.init , isSweepDone = true 调用链中重置为0
	//  gcControllerCommit , isSweepDone = false 调用链中：
	//  GC标记结束时还未START THE WORLD 时，被设置为 GC标记的字节数 + sweepMinHeapDistance (1MB)
	sweepDistMinTrigger atomic.Uint64

	// triggered is the point at which the current GC cycle actually triggered.
	// Only valid during the mark phase of a GC cycle, otherwise set to ^uint64(0).
	//
	// Updated while the world is stopped.
	//
	// startCycle (此时已经STW了)方法中设置为有效值，在 resetLive 中设置为 ^uint64(0)
	// 在清扫终止阶段设置为与触发GC时的trigger不同，因为从test到STW中间 heapLive 的值可能会发生变更。
	//
	// gcMarkTermination -> gcMark -> resetLive 被重置为^uint64(0)。
	//
	// 所以此值只在标记阶段有效。
	triggered uint64

	// lastHeapGoal is the value of heapGoal at the moment the last GC
	// ended. Note that this is distinct from the last value heapGoal had,
	// because it could change if e.g. gcPercent changes.
	//
	// Read and written with the world stopped or with mheap_.lock held.
	//
	// 在 endCycle() 函数调用中赋值为 heapGoal()函数的返回结果，
	// 因为 endCycle() 函数在 commit 函数(更新 gcPercentHeapGoal 的唯一地方)调用之前，
	// 所以记录的是上一轮GC标记结束时的 goal 值(大概率为： gcPercentHeapGoal)。
	// 也是本轮 GC heap Trigger 的比对 trigger 的计算来源 ( goal 和 trigger 往往所差无几)
	// -------------------
	// 在每一轮标记结束时 gcMarkTermination 调用之前更新为 gcPercentHeapGoal。
	// 该字段目前只用来计算 scavenge.gcPercentGoal(当前堆大小如果超过这个值就会进行scavenge) 的值。
	lastHeapGoal uint64

	// heapLive is the number of bytes considered live by the GC.
	// That is: retained by the most recent GC plus allocated
	// since then. heapLive ≤ memstats.totalAlloc-memstats.totalFree, since
	// heapAlloc includes unmarked objects that have not yet been swept (and
	// hence goes up as we allocate and down as we sweep) while heapLive
	// excludes these objects (and hence only goes up between GCs).
	//
	// To reduce contention, this is updated only when obtaining a span
	// from an mcentral and at this point it counts all of the unallocated
	// slots in that span (which will be allocated before that mcache
	// obtains another span from that mcentral). Hence, it slightly
	// overestimates the "true" live heap size. It's better to overestimate
	// than to underestimate because 1) this triggers the GC earlier than
	// necessary rather than potentially too late and 2) this leads to a
	// conservative GC rate rather than a GC rate that is potentially too
	// low.
	//
	// Whenever this is updated, call traceHeapAlloc() and
	// this gcControllerState's revise() method.
	//
	// 在 update 函数中增加，在 resetLive 函数中进行覆盖设置(设置为标记终止阶段总标记字节数)。
	// 每当p从mcentral缓存mspan时，将mspan总的空闲对象总大小用来增加此计数。
	//
	// 在 gcMarkTermination -> gcMark 调用中设置，在 commit 调用之前。设置为被标记的字节数。
	heapLive atomic.Uint64

	// heapScan is the number of bytes of "scannable" heap. This is the
	// live heap (as counted by heapLive), but omitting no-scan objects and
	// no-scan tails of objects.
	//
	// This value is fixed at the start of a GC cycle. It represents the
	// maximum scannable heap.
	//
	// 每一轮标记终止阶段，会在 gcMarkTermination -> gcMark ->  resetLive 函数中设置为 heapScanWork 的值。
	//
	// 起始/重置值：上一轮GC标记结束时被设置为 heapScanWork 的值。
	// 动态增长： 1.非标记阶段，在运行时每当为变量分配内存时，会将变量类型元数据的 ptrData 字段的值累加到此字段。
	//           2.标记阶段，只要此值发生变化就会重新调用此函数，计算新的控制值指导GC步调。
	heapScan atomic.Uint64

	// lastHeapScan is the number of bytes of heap that were scanned
	// last GC cycle. It is the same as heapMarked, but only
	// includes the "scannable" parts of objects.
	//
	// Updated when the world is stopped.
	//
	// 每一轮标记终止阶段，会在 gcMarkTermination -> gcMark ->  resetLive 函数中设置为 heapScanWork 的值。
	// 与 heapScan 不同此值是个快照值不会在其它地方变更。
	lastHeapScan uint64

	// lastStackScan is the number of bytes of stack that were scanned
	// last GC cycle.
	//
	// 只在 gcMarkTermination -> gcMark -> resetLive 函数中设置，设置为字段 stackScanWork 的值。不清零。
	// 是最近一次GC标记终止阶段时扫描的所有g的栈大小之和。(字节)。
	/*
		// scannedSize is the amount of work we'll be reporting.
		//
		// It is less than the allocated size (which is hi-lo).
		var sp uintptr
		if gp.syscallsp != 0 {
			sp = gp.syscallsp // If in a system call this is the stack pointer (gp.sched.sp can be 0 in this case on Windows).
		} else {
			sp = gp.sched.sp
		}
		scannedSize := gp.stack.hi - sp
	*/
	// 所有 _Grunnable 、 _Gwaiting 和 _Gsyscall 状态的g按上面计数到的
	// scannedSize 的总和。
	lastStackScan atomic.Uint64

	// maxStackScan is the amount of allocated goroutine stack space in
	// use by goroutines.
	//
	// This number tracks allocated goroutine stack space rather than used
	// goroutine stack space (i.e. what is actually scanned) because used
	// goroutine stack space is much harder to measure cheaply. By using
	// allocated space, we make an overestimate; this is OK, it's better
	// to conservatively overcount than undercount.
	maxStackScan atomic.Uint64

	// globalsScan is the total amount of global variable space
	// that is scannable.
	// 0  0x000000000101d614 in runtime.(*gcControllerState).addGlobals
	//   at /Users/acorn/workspace/gosource/src/runtime/mgcpacer.go:918
	// 1  0x000000000104dec9 in runtime.modulesinit
	//   at /Users/acorn/workspace/gosource/src/runtime/symtab.go:561
	// 2  0x00000000010367bf in runtime.schedinit
	//   at /Users/acorn/workspace/gosource/src/runtime/proc.go:747
	// 3  0x000000000105e15e in runtime.rt0_go
	//   at /Users/acorn/workspace/gosource/src/runtime/asm_amd64.s:371
	//
	// 只在模块初始化时为此字段添加值，每个模块将其 bss区间和 data 地址返回添加到此字段。
	//
	// 在计算 gcPercentHeapGoal 等时用到此字段的值:
	// 1. gcPercentHeapGoal = c.heapMarked + (c.heapMarked+c.lastStackScan.Load()+c.globalsScan.Load())*uint64(gcPercent)/100
	// 2. scanWorkExpected := int64(c.lastHeapScan + c.lastStackScan.Load() + c.globalsScan.Load())
	// 3. maxScanWork := int64(scan + maxStackScan + c.globalsScan.Load())
	globalsScan atomic.Uint64

	// heapMarked is the number of bytes marked by the previous
	// GC. After mark termination, heapLive == heapMarked, but
	// unlike heapLive, heapMarked does not change until the
	// next mark termination.
	//
	// 在 gcMarkTermination -> gcMark 调用中设置，在 commit 调用之前。
	heapMarked uint64

	// heapScanWork is the total heap scan work performed this cycle.
	// stackScanWork is the total stack scan work performed this cycle.
	// globalsScanWork is the total globals scan work performed this cycle.
	//
	// These are updated atomically during the cycle. Updates occur in
	// bounded batches, since they are both written and read
	// throughout the cycle. At the end of the cycle, heapScanWork is how
	// much of the retained heap is scannable.
	//
	// Currently these are measured in bytes. For most uses, this is an
	// opaque unit of work, but for estimation the definition is important.
	//
	// Note that stackScanWork includes only stack space scanned, not all
	// of the allocated stack.
	// 在标记清扫阶段STW被重置为0。
	//
	// gcControllerState.startCycle 中置0
	heapScanWork atomic.Int64
	/*
		// scannedSize is the amount of work we'll be reporting.
		//
		// It is less than the allocated size (which is hi-lo).
		var sp uintptr
		if gp.syscallsp != 0 {
			sp = gp.syscallsp // If in a system call this is the stack pointer (gp.sched.sp can be 0 in this case on Windows).
		} else {
			sp = gp.sched.sp
		}
		scannedSize := gp.stack.hi - sp
	*/
	// 所有 _Grunnable 、 _Gwaiting 和 _Gsyscall 状态的g按上面计数到的
	// scannedSize 的总和。
	// 在标记清扫阶段STW被重置为0。
	//
	// gcControllerState.startCycle 中置0
	stackScanWork atomic.Int64
	// 在标记清扫阶段STW被重置为0。
	// gcControllerState.startCycle 中置0
	globalsScanWork atomic.Int64

	// bgScanCredit is the scan work credit accumulated by the concurrent
	// background scan. This credit is accumulated by the background scan
	// and stolen by mutator assists.  Updates occur in bounded batches,
	// since it is both written and read throughout the cycle.
	// 在标记清扫阶段STW被重置为0。
	//
	// 存储的扫描的内存大小总和
	// gcControllerState.startCycle 中置0
	bgScanCredit atomic.Int64

	// assistTime is the nanoseconds spent in mutator assists
	// during this cycle. This is updated atomically, and must also
	// be updated atomically even during a STW, because it is read
	// by sysmon. Updates occur in bounded batches, since it is both
	// written and read throughout the cycle.
	// 在标记清扫阶段STW被重置为0。
	// gcControllerState.startCycle 中置0
	assistTime atomic.Int64

	// dedicatedMarkTime is the nanoseconds spent in dedicated mark workers
	// during this cycle. This is updated at the end of the concurrent mark
	// phase.
	// 在标记清扫阶段STW被重置为0。
	// gcControllerState.startCycle 中置0
	dedicatedMarkTime atomic.Int64

	// fractionalMarkTime is the nanoseconds spent in the fractional mark
	// worker during this cycle. This is updated throughout the cycle and
	// will be up-to-date if the fractional mark worker is not currently
	// running.
	// 在标记清扫阶段STW被重置为0。
	// gcControllerState.startCycle 中置0
	fractionalMarkTime atomic.Int64

	// idleMarkTime is the nanoseconds spent in idle marking during this
	// cycle. This is updated throughout the cycle.
	// 在标记清扫阶段STW被重置为0。
	// gcControllerState.startCycle 中置0
	idleMarkTime atomic.Int64

	// markStartTime is the absolute start time in nanoseconds
	// that assists and background mark workers started.
	//
	// gcControllerState.startCycle 中置为stop the world调用之前的时间戳。
	markStartTime int64

	// dedicatedMarkWorkersNeeded is the number of dedicated mark workers
	// that need to be started. This is computed at the beginning of each
	// cycle and decremented as dedicated mark workers get started.
	//
	// 在标记终止阶段STW状态下被设置， gcStart -> startCycle
	dedicatedMarkWorkersNeeded atomic.Int64

	// idleMarkWorkers is two packed int32 values in a single uint64.
	// These two values are always updated simultaneously.
	//
	// The bottom int32 is the current number of idle mark workers executing.
	//
	// The top int32 is the maximum number of idle mark workers allowed to
	// execute concurrently. Normally, this number is just gomaxprocs. However,
	// during periodic GC cycles it is set to 0 because the system is idle
	// anyway; there's no need to go full blast on all of GOMAXPROCS.
	//
	// The maximum number of idle mark workers is used to prevent new workers
	// from starting, but it is not a hard maximum. It is possible (but
	// exceedingly rare) for the current number of idle mark workers to
	// transiently exceed the maximum. This could happen if the maximum changes
	// just after a GC ends, and an M with no P.
	//
	// Note that if we have no dedicated mark workers, we set this value to
	// 1 in this case we only have fractional GC workers which aren't scheduled
	// strictly enough to ensure GC progress. As a result, idle-priority mark
	// workers are vital to GC progress in these situations.
	//
	// For example, consider a situation in which goroutines block on the GC
	// (such as via runtime.GOMAXPROCS) and only fractional mark workers are
	// scheduled (e.g. GOMAXPROCS=1). Without idle-priority mark workers, the
	// last running M might skip scheduling a fractional mark worker if its
	// utilization goal is met, such that once it goes to sleep (because there's
	// nothing to do), there will be nothing else to spin up a new M for the
	// fractional worker in the future, stalling GC progress and causing a
	// deadlock. However, idle-priority workers will *always* run when there is
	// nothing left to do, ensuring the GC makes progress.
	//
	// See github.com/golang/go/issues/44163 for more details.
	//
	// 低32位存储当前 idleMarkWorker 的数量，高32位存储 idleMarkWorker 允许的最大值。
	//
	// idleMarkWorkers 的最大值在GC的扫描终止阶段STW中被设置， startCycle 函数中被设置。
	// 对于非 gcTriggerTimer 类型的 trigger 设置为 goMaxProcs - dedicatedMarkWorkersNeeded
	// gcTriggerTimer 类型的 trigger 当 dedicatedMarkWorkersNeeded > 0 时设置为0，否则设置为1.
	idleMarkWorkers atomic.Uint64

	// assistWorkPerByte is the ratio of scan work to allocated
	// bytes that should be performed by mutator assists. This is
	// computed at the beginning of each cycle and updated every
	// time heapScan is updated.
	//
	// 与 assistBytesPerWork 字段的值互为倒数。
	assistWorkPerByte atomic.Float64

	// assistBytesPerWork is 1/assistWorkPerByte.
	//
	// Note that because this is read and written independently
	// from assistWorkPerByte users may notice a skew between
	// the two values, and such a state should be safe.
	assistBytesPerWork atomic.Float64

	// fractionalUtilizationGoal is the fraction of wall clock
	// time that should be spent in the fractional mark worker on
	// each P that isn't running a dedicated worker.
	//
	// For example, if the utilization goal is 25% and there are
	// no dedicated workers, this will be 0.25. If the goal is
	// 25%, there is one dedicated worker, and GOMAXPROCS is 5,
	// this will be 0.05 to make up the missing 5%.
	//
	// If this is zero, no fractional workers are needed.
	fractionalUtilizationGoal float64

	// These memory stats are effectively duplicates of fields from
	// memstats.heapStats but are updated atomically or with the world
	// stopped and don't provide the same consistency guarantees.
	//
	// Because the runtime is responsible for managing a memory limit, it's
	// useful to couple these stats more tightly to the gcController, which
	// is intimately connected to how that memory limit is maintained.
	//
	// 分配的所有mspan的总大小，至少有一个obj被使用。
	// spanAllocHeap 类型的msapn，在 mheap.grow 方法中增加，在 mheap.freeSpanLocked 中
	// 减少。
	heapInUse    sysMemStat    // bytes in mSpanInUse spans
	// 页面对应的 pageAlloc.chunks 的scavenged位是1。
	heapReleased sysMemStat    // bytes released to the OS
	// 页面对应的 pageAlloc.chunks 的scavenged位是0，alloc位也为0。
	heapFree     sysMemStat    // bytes not in any span, but not released to the OS
	totalAlloc   atomic.Uint64 // total bytes allocated
	// 字节数
	// 比如mspan总别释放的3个obj，那么对此值的贡献就是 3*s.elemsize
	totalFree    atomic.Uint64 // total bytes freed
	// 分配位为0/1，scavenged位为0的页的总大小。
	mappedReady  atomic.Uint64 // total virtual memory in the Ready state (see mem.go).

	// test indicates that this is a test-only copy of gcControllerState.
	test bool

	_ cpu.CacheLinePad
}

func (c *gcControllerState) init(gcPercent int32, memoryLimit int64) {
	c.heapMinimum = defaultHeapMinimum
	c.triggered = ^uint64(0)
	c.setGCPercent(gcPercent)
	c.setMemoryLimit(memoryLimit)
	c.commit(true) // No sweep phase in the first GC cycle.
	// N.B. Don't bother calling traceHeapGoal. Tracing is never enabled at
	// initialization time.
	// N.B. No need to call revise; there's no GC enabled during
	// initialization.
}

// startCycle resets the GC controller's state and computes estimates
// for a new GC cycle. The caller must hold worldsema and the world
// must be stopped.
//
// startCycle()
// 在 gcStart 函数中被调用，紧随 systemstack(stopTheWorldWithSema) 调用之后。
// 主要目的：
// 1. 重置 gcControllerState 中一些记录GC相关统计信息的字段。
// 2. 并计数一些GC需要的数值。比如 dedicatedWorker的数量 和 fractinal work 运行时间的占比等。
// 3. 调用 revise
func (c *gcControllerState) startCycle(markStartTime int64, procs int, trigger gcTrigger) {
	c.heapScanWork.Store(0)
	c.stackScanWork.Store(0)
	c.globalsScanWork.Store(0)
	c.bgScanCredit.Store(0)
	c.assistTime.Store(0)
	c.dedicatedMarkTime.Store(0)
	c.fractionalMarkTime.Store(0)
	c.idleMarkTime.Store(0)
	c.markStartTime = markStartTime
	c.triggered = c.heapLive.Load()

	// Compute the background mark utilization goal. In general,
	// this may not come out exactly. We round the number of
	// dedicated workers so that the utilization is closest to
	// 25%. For small GOMAXPROCS, this would introduce too much
	// error, so we add fractional workers in that case.
	totalUtilizationGoal := float64(procs) * gcBackgroundUtilization
	dedicatedMarkWorkersNeeded := int64(totalUtilizationGoal + 0.5)
	utilError := float64(dedicatedMarkWorkersNeeded)/totalUtilizationGoal - 1
	const maxUtilError = 0.3
	// 修正后的结果：
	// GOMAXPROCS    dedicatedMarkWorkersNeeded  fractionalUtilizationGoal
	//   1                 0                         0.25
	//   2                 0                         0.25
	//   3                 0                         0.25
	//   4                 1					     0
	//   5                 1                         0
	//   6                 1                         0.083
	//   7                 2                         0
	//   8                 2                         0
	//   9                 2                         0
	//   10                3                         0
	//   11                3                         0
	//   12                3                         0
	//   13                3                         0
	//   14                4                         0
	//   15                4                         0
	//   16                4                         0
	//   >16               floor(GOMAXPROCS*gcBackgroundUtilization(0.25)) 0
	if utilError < -maxUtilError || utilError > maxUtilError {
		// 修正后的worker数比原来大30%或小30%往上。
		//
		// Rounding put us more than 30% off our goal. With
		// gcBackgroundUtilization of 25%, this happens for
		// GOMAXPROCS<=3 or GOMAXPROCS=6. Enable fractional
		// workers to compensate.
		if float64(dedicatedMarkWorkersNeeded) > totalUtilizationGoal {
			// Too many dedicated workers.
			dedicatedMarkWorkersNeeded--
		}
		// 至此 dedicatedMarkWorkersNeeded 肯定小于 totalUtilizationGoal
		c.fractionalUtilizationGoal = (totalUtilizationGoal - float64(dedicatedMarkWorkersNeeded)) / float64(procs)
	} else {
		c.fractionalUtilizationGoal = 0
	}

	// In STW mode, we just want dedicated workers.
	if debug.gcstoptheworld > 0 {
		dedicatedMarkWorkersNeeded = int64(procs)
		c.fractionalUtilizationGoal = 0
	}

	// Clear per-P state
	for _, p := range allp {
		p.gcAssistTime = 0
		p.gcFractionalMarkTime = 0
	}

	if trigger.kind == gcTriggerTime {
		// 周期型的GC，如果dedicatedMarkWorkersNeeded大于0，将idleMarkWorkers设置为0。
		// 否则将idleMarkWorkers设置为1。
		//
		// During a periodic GC cycle, reduce the number of idle mark workers
		// required. However, we need at least one dedicated mark worker or
		// idle GC worker to ensure GC progress in some scenarios (see comment
		// on maxIdleMarkWorkers).
		if dedicatedMarkWorkersNeeded > 0 {
			c.setMaxIdleMarkWorkers(0)
		} else {
			// TODO(mknyszek): The fundamental reason why we need this is because
			// we can't count on the fractional mark worker to get scheduled.
			// Fix that by ensuring it gets scheduled according to its quota even
			// if the rest of the application is idle.
			c.setMaxIdleMarkWorkers(1)
		}
	} else {
		// 非周期类型GC
		//
		// N.B. gomaxprocs and dedicatedMarkWorkersNeeded are guaranteed not to
		// change during a GC cycle.
		c.setMaxIdleMarkWorkers(int32(procs) - int32(dedicatedMarkWorkersNeeded))
	}

	// Compute initial values for controls that are updated
	// throughout the cycle.
	c.dedicatedMarkWorkersNeeded.Store(dedicatedMarkWorkersNeeded)
	// 计算一些初始值，这些初始值会在接下来的标记阶段被更新。
	c.revise()

	if debug.gcpacertrace > 0 {
		heapGoal := c.heapGoal()
		assistRatio := c.assistWorkPerByte.Load()
		print("pacer: assist ratio=", assistRatio,
			" (scan ", gcController.heapScan.Load()>>20, " MB in ",
			work.initialHeapLive>>20, "->",
			heapGoal>>20, " MB)",
			" workers=", dedicatedMarkWorkersNeeded,
			"+", c.fractionalUtilizationGoal, "\n")
	}
}

// revise updates the assist ratio during the GC cycle to account for
// improved estimates. This should be called whenever gcController.heapScan,
// gcController.heapLive, or if any inputs to gcController.heapGoal are
// updated. It is safe to call concurrently, but it may race with other
// calls to revise.
//
// The result of this race is that the two assist ratio values may not line
// up or may be stale. In practice this is OK because the assist ratio
// moves slowly throughout a GC cycle, and the assist ratio is a best-effort
// heuristic anyway.
//
// Furthermore, no part of the heuristic depends on
// the two assist ratio values being exact reciprocals of one another, since
// the two values are used to convert values from different sources.
//
// The worst case result of this raciness is that we may miss a larger shift
// in the ratio (say, if we decide to pace more aggressively against the
// hard heap goal) but even this "hard goal" is best-effort (see #40460).
// The dedicated GC should ensure we don't exceed the hard goal by too much
// in the rare case we do exceed it.
//
// It should only be called when gcBlackenEnabled != 0 (because this
// is when assists are enabled and the necessary statistics are
// available).
//
// ** 此函数最终任务是实时更新 assistWorkPerByte 和 assistBytesPerWork，指导标记阶段分配/标记的步调。
// ** 每当计算这两个比率的输入数值发生变更时都会调用此函数。
// 目前会在 mcache.refill mcache.allocLarge 和 mcache.releaseAll(标记阶段不大可能调用此函数)
// mcache.refill 与 mcache.allocLarge 都是为对象分配内存时p上对应的缓存mspan没有空闲对象时，必
// 需从mcentral或者直接从页堆上分配时才会调的函数。
//
// 其实也很好理解，新对象的虽然不会增加标记的工作量(因为标记阶段新申请的内存会直接标记为黑色)，但会延长标记的时间
// 推迟sweep的时机，整个堆的大小会不受控，使GC步调变得不平稳和不可预测。
//
// *** 在标记结束时，堆大小(heapLive)最多是触发本轮GC时(heapLive)的2倍。
// var Dup map[uint64]bool
func (c *gcControllerState) revise() {
	gcPercent := c.gcPercent.Load()
	if gcPercent < 0 {
		// If GC is disabled but we're running a forced GC,
		// act like GOGC is huge for the below calculations.
		gcPercent = 100000
	}
	// ** 下面的动态更新指的是指两次调用此函数时，下面变量初始源值很可能会不一样。
	// live 动态变化，只会增加。
	//
	// 起始/重置值： 上一轮GC标记结束时被设置为标记的字节数
	// 动态增长：在运行时:
	// 每当p从mcentral缓存mspan时，将mspan总的空闲对象总大小用来增加此计数。
	live := c.heapLive.Load()
	// scan 动态变化，只会增加
	//
	// 起始/重置值：上一轮GC标记结束时被设置为 heapScanWork 的值。
	// 动态增长：1.非标记阶段，在运行时每当为变量分配内存时，会将变量类型元数据的ptrData字段的值累加到此字段。
	//         2.标记阶段，1中的情况只会导致调用此函数，而不会增加计数。
	scan := c.heapScan.Load()
	// 动态变化，只会增加
	//
	// 起始值：0+0+模块的bss和data区间大小。
	// 动态增长：随着heapScanWork 和 stackScanWork 变化而变化。
	work := c.heapScanWork.Load() + c.stackScanWork.Load() + c.globalsScanWork.Load()

	// Assume we're under the soft goal. Pace GC to complete at
	// heapGoal assuming the heap is in steady-state.
	//
	// 起始值：上一轮GC标记结束时，被标记的字节数 + 协程栈使用区间总和 + 模块bss和data区间之和。
	// 动态更新：标记阶段如果调用了此函数会动态更新。
	heapGoal := int64(c.heapGoal())

	// The expected scan work is computed as the amount of bytes scanned last
	// GC cycle (both heap and stack), plus our estimate of globals work for this cycle.
	//
	// 上一轮标记结束时的：heapScanWork + stackScanWork + 模块bss和data区间之和。
	// 动态更新：在运行中不会动态更新。
	scanWorkExpected := int64(c.lastHeapScan + c.lastStackScan.Load() + c.globalsScan.Load())

	// maxScanWork is a worst-case estimate of the amount of scan work that
	// needs to be performed in this GC cycle. Specifically, it represents
	// the case where *all* scannable memory turns out to be live, and
	// *all* allocated stack space is scannable.
	//
	// 起始值：没有起始值，不会被重置。
	// 动态更新：每当有协程被创时，会将其 g.stack.hi - g.stack.lo 添加到此字段，每当
	//         协程退出时会减去 g.stack.hi-g.stack.lo ，每当协程的栈被复制时减去旧
	//         栈的大小。
	maxStackScan := c.maxStackScan.Load()
	// DDup(maxStackScan)
	// Dup[maxStackScan]=true
	// maxStackScan
	// 最多被扫描的字节数，实际扫描到的字节数肯定会比这个少。
	// 随着 maxStackSan 的动态更新而更新。
	maxScanWork := int64(scan + maxStackScan + c.globalsScan.Load())
	// 本轮目前已扫描的工作量已经大于上一轮GC扫描的任务量。
	// 如果上一轮标记结束时，被标记的对象+到此轮GC开始时新增的对象 > 上一轮标记结束时扫描的任务量
	// 就会出现这种情况。
	DDup(uint64(heapGoal))
	if work > scanWorkExpected {
		println("heapGoal",heapGoal)
		println("live",live)
		println("maxScanWork",maxScanWork)
		println("work",work)
		println("diff",maxScanWork-work)
		println("scan",scan)
		// println("work>scanWOrkExpected")
		// 在标记阶段:
		// 1. c.heapScan不会增加计数。
		// 2. 建的栈也不会增加c.stackScanWork计数。
		// 所以一旦scanWorkExpected被赋值为maxScanWork，便不会再出现work大于scanWorkExpected的情况。
		//
		// We've already done more scan work than expected. Because our expectation
		// is based on a steady-state scannable heap size, we assume this means our
		// heap is growing.
		//
		// Compute a new heap goal that takes our existing runway
		// computed for scanWorkExpected and extrapolates it to maxScanWork, the worst-case
		// scan work. This keeps our assist ratio stable if the heap continues to grow.
		//
		// The effect of this mechanism is that assists stay flat in the face of heap
		// growths. It's OK to use more memory this cycle to scan all the live heap,
		// because the next GC cycle is inevitably going to use *at least* that much
		// memory anyway.
		//
		// GC开始时在标记阶段之前 heapGoal=c.triggered，在标记阶段heapGoal的源值会被更新。
		extHeapGoal := int64(float64(heapGoal-int64(c.triggered))/float64(scanWorkExpected)*float64(maxScanWork)) + int64(c.triggered)
		scanWorkExpected = maxScanWork

		// hardGoal is a hard limit on the amount that we're willing to push back the
		// heap goal, and that's twice the heap goal (i.e. if GOGC=100 and the heap and/or
		// stacks and/or globals grow to twice their size, this limits the current GC cycle's
		// growth to 4x the original live heap's size).
		//
		// 因为上面的heapGoal可能会增加到c.triggered的2倍。而hardGoal又是heapGoal的二倍。
		// 所以hardGoal最多是c.triggered的四倍。
		//
		// This maintains the invariant that we use no more memory than the next GC cycle
		// will anyway.
		hardGoal := int64((1.0 + float64(gcPercent)/100.0) * float64(heapGoal))
		if extHeapGoal > hardGoal {
			extHeapGoal = hardGoal
		}
		heapGoal = extHeapGoal
		println("new heapgoal",heapGoal)
		println("hard goal",hardGoal)
	}
	if int64(live) > heapGoal {
		// We're already past our heap goal, even the extrapolated one.
		// Leave ourselves some extra runway, so in the worst case we
		// finish by that point.
		const maxOvershoot = 1.1
		heapGoal = int64(float64(heapGoal) * maxOvershoot)

		// Compute the upper bound on the scan work remaining.
		scanWorkExpected = maxScanWork
	}

	// Compute the remaining scan work estimate.
	//
	// Note that we currently count allocations during GC as both
	// scannable heap (heapScan) and scan work completed
	// (scanWork), so allocation will change this difference
	// slowly in the soft regime and not at all in the hard
	// regime.
	//
	scanWorkRemaining := scanWorkExpected - work
	if scanWorkRemaining < 1000 {
		// We set a somewhat arbitrary lower bound on
		// remaining scan work since if we aim a little high,
		// we can miss by a little.
		//
		// We *do* need to enforce that this is at least 1,
		// since marking is racy and double-scanning objects
		// may legitimately make the remaining scan work
		// negative, even in the hard goal regime.
		scanWorkRemaining = 1000
	}

	// Compute the heap distance remaining.
	//
	// 标记阶段还允许分配的内存。
	heapRemaining := heapGoal - int64(live)
	if heapRemaining <= 0 {
		// This shouldn't happen, but if it does, avoid
		// dividing by zero or setting the assist negative.
		heapRemaining = 1
	}

	// Compute the mutator assist ratio so by the time the mutator
	// allocates the remaining heap bytes up to heapGoal, it will
	// have done (or stolen) the remaining amount of scan work.
	// Note that the assist ratio values are updated atomically
	// but not together. This means there may be some degree of
	// skew between the two values. This is generally OK as the
	// values shift relatively slowly over the course of a GC
	// cycle.
	//
	// 因为assistWorkPerByte和assistBytesPerWork的读取不是同时的，所以一个辅助GC
	// 使用的这两个值可能不一定互为倒数。
	// 不过上面的注释也说了，这种扭曲是轻微的。
	assistWorkPerByte := float64(scanWorkRemaining) / float64(heapRemaining)
	assistBytesPerWork := float64(heapRemaining) / float64(scanWorkRemaining)
	println(assistBytesPerWork,assistWorkPerByte)
	c.assistWorkPerByte.Store(assistWorkPerByte)
	c.assistBytesPerWork.Store(assistBytesPerWork)
}

// 标记结束时执行。
// 在 gcMarkDone 函数中执行 且于 gcMarkTermination 之前之前。
//
// userForced 是否是周期性触发的GC
//
// endCycle computes the consMark estimate for the next cycle.
// userForced indicates whether the current GC cycle was forced
// by the application.
func (c *gcControllerState) endCycle(now int64, procs int, userForced bool) {
	// Record last heap goal for the scavenger.
	// We'll be updating the heap goal soon.
	//
	// 本轮GC触发时，hapGoal的值。一般比trigger值稍大一点点。
	gcController.lastHeapGoal = c.heapGoal()

	// Compute the duration of time for which assists were turned on.
	//
	// c.markStartTime在startCycle中设置为清扫终止阶段的STW调用之前的时间。
	assistDuration := now - c.markStartTime

	// Assume background mark hit its utilization goal.
	utilization := gcBackgroundUtilization
	// Add assist utilization; avoid divide by zero.
	if assistDuration > 0 {
		utilization += float64(c.assistTime.Load()) / float64(assistDuration*int64(procs))
	}
	// utilization=执行dedicated worker的工作时间占比+0.25

	if c.heapLive.Load() <= c.triggered {
		// c.heapLive此时的值应比触发本轮GC时c.heapLive的值大。
		//
		// Shouldn't happen, but let's be very safe about this in case the
		// GC is somehow extremely short.
		//
		// In this case though, the only reasonable value for c.heapLive-c.triggered
		// would be 0, which isn't really all that useful, i.e. the GC was so short
		// that it didn't matter.
		//
		// Ignore this case and don't update anything.
		return
	}
	// idleWorker工作时长占比
	idleUtilization := 0.0
	if assistDuration > 0 {
		idleUtilization = float64(c.idleMarkTime.Load()) / float64(assistDuration*int64(procs))
	}
	// Determine the cons/mark ratio.
	//
	// The units we want for the numerator and denominator are both B / cpu-ns.
	// We get this by taking the bytes allocated or scanned, and divide by the amount of
	// CPU time it took for those operations. For allocations, that CPU time is
	//
	//    assistDuration * procs * (1 - utilization)
	//
	// Where utilization includes just background GC workers and assists. It does *not*
	// include idle GC work time, because in theory the mutator is free to take that at
	// any point.
	//
	// For scanning, that CPU time is
	//
	//    assistDuration * procs * (utilization + idleUtilization)
	//
	// In this case, we *include* idle utilization, because that is additional CPU time that
	// the GC had available to it.
	//
	// In effect, idle GC time is sort of double-counted here, but it's very weird compared
	// to other kinds of GC work, because of how fluid it is. Namely, because the mutator is
	// *always* free to take it.
	//
	// So this calculation is really:
	//     (heapLive-trigger) / (assistDuration * procs * (1-utilization)) /
	//         (scanWork) / (assistDuration * procs * (utilization+idleUtilization)
	//
	// Note that because we only care about the ratio, assistDuration and procs cancel out.
	scanWork := c.heapScanWork.Load() + c.stackScanWork.Load() + c.globalsScanWork.Load()
	// currentConsMark 分配速率与标记速率的比值
	currentConsMark := (float64(c.heapLive.Load()-c.triggered) * (utilization + idleUtilization)) /
		(float64(scanWork) * (1 - utilization))

	// Update our cons/mark estimate. This is the raw value above, but averaged over 2 GC cycles
	// because it tends to be jittery, even in the steady-state. The smoothing helps the GC to
	// maintain much more stable cycle-by-cycle behavior.
	oldConsMark := c.consMark
	// 取两次的平均值作为c.consMark的值
	c.consMark = (currentConsMark + c.lastConsMark) / 2
	c.lastConsMark = currentConsMark

	if debug.gcpacertrace > 0 {
		printlock()
		goal := gcGoalUtilization * 100
		// pacer: 0.25+dedicated worker工作时长占比。
		// goal: 0.25*100
		// 此轮GC标记堆中被扫描的内存大小 + 栈被扫描的内存大小(hi-sp) + bss/data被扫描的大小
		// ( 上一次上面三类扫描总和
		// 此轮GC触发时堆大小-> 标记结束时堆大小 标记结束时堆大小-触发本轮GC时的goal。
		// cons/mark 上一轮GC标记结束时计算出来的gcController.consMark。
		print("pacer: ", int(utilization*100), "% CPU (", int(goal), " exp.) for ")
		print(c.heapScanWork.Load(), "+", c.stackScanWork.Load(), "+", c.globalsScanWork.Load(), " B work (", c.lastHeapScan+c.lastStackScan.Load()+c.globalsScan.Load(), " B exp.) ")
		live := c.heapLive.Load()
		print("in ", c.triggered, " B -> ", live, " B (∆goal ", int64(live)-int64(c.lastHeapGoal), ", cons/mark ", oldConsMark, ")")
		println()
		printunlock()
	}
}

// enlistWorker encourages another dedicated mark worker to start on
// another P if there are spare worker slots. It is used by putfull
// when more work is made available.
//
//go:nowritebarrier
func (c *gcControllerState) enlistWorker() {
	// If there are idle Ps, wake one so it will run an idle worker.
	// NOTE: This is suspected of causing deadlocks. See golang.org/issue/19112.
	//
	//	if sched.npidle.Load() != 0 && sched.nmspinning.Load() == 0 {
	//		wakep()
	//		return
	//	}

	// There are no idle Ps. If we need more dedicated workers,
	// try to preempt a running P so it will switch to a worker.
	if c.dedicatedMarkWorkersNeeded.Load() <= 0 {
		return
	}
	// Pick a random other P to preempt.
	if gomaxprocs <= 1 {
		return
	}
	gp := getg()
	if gp == nil || gp.m == nil || gp.m.p == 0 {
		return
	}
	myID := gp.m.p.ptr().id
	for tries := 0; tries < 5; tries++ {
		id := int32(fastrandn(uint32(gomaxprocs - 1)))
		if id >= myID {
			id++
		}
		p := allp[id]
		if p.status != _Prunning {
			continue
		}
		if preemptone(p) {
			return
		}
	}
}

// findRunnableGCWorker returns a background mark worker for pp if it
// should be run. This must only be called when gcBlackenEnabled != 0.
func (c *gcControllerState) findRunnableGCWorker(pp *p, now int64) (*g, int64) {
	if gcBlackenEnabled == 0 {
		throw("gcControllerState.findRunnable: blackening not enabled")
	}

	// Since we have the current time, check if the GC CPU limiter
	// hasn't had an update in a while. This check is necessary in
	// case the limiter is on but hasn't been checked in a while and
	// so may have left sufficient headroom to turn off again.
	if now == 0 {
		now = nanotime()
	}
	if gcCPULimiter.needUpdate(now) {
		gcCPULimiter.update(now)
	}

	if !gcMarkWorkAvailable(pp) {
		// No work to be done right now. This can happen at
		// the end of the mark phase when there are still
		// assists tapering off. Don't bother running a worker
		// now because it'll just return immediately.
		return nil, now
	}

	// Grab a worker before we commit to running below.
	node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
	if node == nil {
		// There is at least one worker per P, so normally there are
		// enough workers to run on all Ps, if necessary. However, once
		// a worker enters gcMarkDone it may park without rejoining the
		// pool, thus freeing a P with no corresponding worker.
		// gcMarkDone never depends on another worker doing work, so it
		// is safe to simply do nothing here.
		//
		// If gcMarkDone bails out without completing the mark phase,
		// it will always do so with queued global work. Thus, that P
		// will be immediately eligible to re-run the worker G it was
		// just using, ensuring work can complete.
		return nil, now
	}

	// decIfPositive 基于原子指令CAS实现一个正整数减一的操作，被用于分配
	// dedicated 模式的worker，优先返回 dedicated 模式的worker。
	decIfPositive := func(val *atomic.Int64) bool {
		for {
			v := val.Load()
			if v <= 0 {
				return false
			}

			if val.CompareAndSwap(v, v-1) {
				return true
			}
		}
	}
	// c.fractionalUtilizationGoal 和 c.dedicatedMarkWorkersNeeded
	// 都是本轮GC开始时计算出来的，分别表示dedicated 、 fractional模式的
	// worker会占用多少个P。

	if decIfPositive(&c.dedicatedMarkWorkersNeeded) {
		// This P is now dedicated to marking until the end of
		// the concurrent mark phase.
		pp.gcMarkWorkerMode = gcMarkWorkerDedicatedMode
	} else if c.fractionalUtilizationGoal == 0 {
		// No need for fractional workers.
		gcBgMarkWorkerPool.push(&node.node)
		return nil, now
	} else {
		// Is this P behind on the fractional utilization
		// goal?
		//
		// This should be kept in sync with pollFractionalWorkerExit.
		//
		// delta 是从本轮开始标记已经过去的时间，用当前P的 gcFractionalMarkTime 除以 delta 得到
		// 的是当前P运行 fractional worker 所花时间占总时间的百分比，这样可以把 fractional worker
		// 分摊到所有P上去执行，尽量使每个P都均匀地分摊任务。如果当前P的执行时间
		// 已经超过目标值就返回nil。
		delta := now - c.markStartTime
		if delta > 0 && float64(pp.gcFractionalMarkTime)/float64(delta) > c.fractionalUtilizationGoal {
			// Nope. No need to run a fractional worker.
			gcBgMarkWorkerPool.push(&node.node)
			return nil, now
		}
		// Run a fractional worker.
		pp.gcMarkWorkerMode = gcMarkWorkerFractionalMode
	}

	// Run the background mark worker.
	gp := node.gp.ptr()
	casgstatus(gp, _Gwaiting, _Grunnable)
	if trace.enabled {
		traceGoUnpark(gp, 0)
	}
	return gp, now
}

// resetLive 函数只在在 gcMark 函数中调用。在 commit 之前被调用。
//
// resetLive sets up the controller state for the next mark phase after the end
// of the previous one. Must be called after endCycle and before commit, before
// the world is started.
//
// The world must be stopped.
func (c *gcControllerState) resetLive(bytesMarked uint64) {
	c.heapMarked = bytesMarked
	c.heapLive.Store(bytesMarked)
	c.heapScan.Store(uint64(c.heapScanWork.Load()))
	c.lastHeapScan = uint64(c.heapScanWork.Load())
	c.lastStackScan.Store(uint64(c.stackScanWork.Load()))
	c.triggered = ^uint64(0) // Reset triggered.

	// heapLive was updated, so emit a trace event.
	if trace.enabled {
		traceHeapAlloc(bytesMarked)
	}
}

// markWorkerStop must be called whenever a mark worker stops executing.
//
// It updates mark work accounting in the controller by a duration of
// work in nanoseconds and other bookkeeping.
//
// Safe to execute at any time.
func (c *gcControllerState) markWorkerStop(mode gcMarkWorkerMode, duration int64) {
	switch mode {
	case gcMarkWorkerDedicatedMode:
		c.dedicatedMarkTime.Add(duration)
		c.dedicatedMarkWorkersNeeded.Add(1)
	case gcMarkWorkerFractionalMode:
		c.fractionalMarkTime.Add(duration)
	case gcMarkWorkerIdleMode:
		c.idleMarkTime.Add(duration)
		c.removeIdleMarkWorker()
	default:
		throw("markWorkerStop: unknown mark worker mode")
	}
}

// update()
//
//	gcController.update(int64(s.npages*pageSize)-int64(usedBytes), int64(c.scanAlloc))
//
// dHeapLive 是新申请的 mspan 中的空闲字节数。
// dHeapScan mcache 中已分配的 scanalbe 类型的字节数。等于分配的 noscan span class 类的obj总共字节数。
// 此函数调用之后， mcache.scanAlloc 会清零。
func (c *gcControllerState) update(dHeapLive, dHeapScan int64) {
	if dHeapLive != 0 {
		live := gcController.heapLive.Add(dHeapLive)
		if trace.enabled {
			// gcController.heapLive changed.
			traceHeapAlloc(live)
		}
	}
	if gcBlackenEnabled == 0 {
		// Update heapScan when we're not in a current GC. It is fixed
		// at the beginning of a cycle.
		if dHeapScan != 0 {
			gcController.heapScan.Add(dHeapScan)
		}
	} else {
		// gcController.heapLive changed.
		c.revise()
	}
}

func (c *gcControllerState) addScannableStack(pp *p, amount int64) {
	if pp == nil {
		c.maxStackScan.Add(amount)
		return
	}
	pp.maxStackScanDelta += amount
	if pp.maxStackScanDelta >= maxStackScanSlack || pp.maxStackScanDelta <= -maxStackScanSlack {
		c.maxStackScan.Add(pp.maxStackScanDelta)
		pp.maxStackScanDelta = 0
	}
}

func (c *gcControllerState) addGlobals(amount int64) {
	c.globalsScan.Add(amount)
}

// heapGoal returns the current heap goal.
func (c *gcControllerState) heapGoal() uint64 {
	goal, _ := c.heapGoalInternal()
	return goal
}

// heapGoalInternal is the implementation of heapGoal which returns additional
// information that is necessary for computing the trigger.
//
// The returned minTrigger is always <= goal.
//
// heapGoalInternal() 此函数在 heapGoal 和 trigger 函数中被调用。
//
// trigger()调用路线中：
// 返回值：
// goal: gcControllerState.gcPercentHeapGoal(没开启MEMORYLIMIT时)
// minTrigger: gcControllerState.sweepDistMinTrigger
//
// minTrigger <= goal
func (c *gcControllerState) heapGoalInternal() (goal, minTrigger uint64) {
	// Start with the goal calculated for gcPercent.
	goal = c.gcPercentHeapGoal.Load()

	// Check if the memory-limit-based goal is smaller, and if so, pick that.
	if newGoal := c.memoryLimitHeapGoal(); go119MemoryLimitSupport && newGoal < goal {
		// 没开启MEMORYLIMIT时，不会执行到这里
		goal = newGoal
	} else {
		// We're not limited by the memory limit goal, so perform a series of
		// adjustments that might move the goal forward in a variety of circumstances.

		sweepDistTrigger := c.sweepDistMinTrigger.Load()
		// goal 最小值是4MB
		// 正常来说 goal 是大于 sweepDistTrigger， 除非GOGC设置的非常小。
		// 因为 sweepDistTrigger 的计算依赖GC标记的字节数 + sweepMinHeapDistance(1MB)，
		// 而 goal 依赖于 GC标记字节数 + (GC标记字节数+一些统计量)*GOGC。
		if sweepDistTrigger > goal {
			// Set the goal to maintain a minimum sweep distance since
			// the last call to commit. Note that we never want to do this
			// if we're in the memory limit regime, because it could push
			// the goal up.
			goal = sweepDistTrigger
		}
		// Since we ignore the sweep distance trigger in the memory
		// limit regime, we need to ensure we don't propagate it to
		// the trigger, because it could cause a violation of the
		// invariant that the trigger < goal.
		minTrigger = sweepDistTrigger

		// Ensure that the heap goal is at least a little larger than
		// the point at which we triggered. This may not be the case if GC
		// start is delayed or if the allocation that pushed gcController.heapLive
		// over trigger is large or if the trigger is really close to
		// GOGC. Assist is proportional to this distance, so enforce a
		// minimum distance, even if it means going over the GOGC goal
		// by a tiny bit.
		//
		// Ignore this if we're in the memory limit regime: we'd prefer to
		// have the GC respond hard about how close we are to the goal than to
		// push the goal back in such a manner that it could cause us to exceed
		// the memory limit.
		//
		// 64KB
		// minRunway distance，让goal比trigger大一点。
		const minRunway = 64 << 10
		// c.triggered 是上一次GC开始时，扫描终止阶段的 c.heapLive 的大小。[startCycle函数中]
		// 在标记结束时 c.triggered 会被设置为 ^uint64(0) [resetLive函数中]
		//
		// 所以验证heap GC触发是否满足时此字段值为 ^uint64(0)
		if c.triggered != ^uint64(0) && goal < c.triggered+minRunway {
			// 只会在标记阶段执行到这里，因为标记结束后 c.triggered的值是^uint64(0)
			goal = c.triggered + minRunway
		}
	}
	return
}

// memoryLimitHeapGoal returns a heap goal derived from memoryLimit.
// memoryLimitHeapGoalHeadroom = 1MB
//
// 根据MEMORYLIMIT的值计算heapGoal。
//
// heapGoal 的值按一下计算：
// 1. nonHeapMemory+overage >= memoryLimit ; 返回 gcControllerState.heapMarked
// 2. 因为memory limit限制的是mapped页的总大小，而goal是目标堆大小。所以计算目标值首先要减去非堆内存，然后再保守的减去超额(认为超额都是堆造成的)：
//    还有减去1MB的headerRoom。
// overage = max(mappedReady - memoryLimit,0)
// goal := memoryLimit - (nonHeapMemory + overage)
// 	if goal < memoryLimitHeapGoalHeadroom || goal-memoryLimitHeapGoalHeadroom < memoryLimitHeapGoalHeadroom {
//		goal = memoryLimitHeapGoalHeadroom
//	} else {
//		goal = goal - memoryLimitHeapGoalHeadroom
//	}
//
// 最小值为 gcControllerState.heapMarked。小于heapMarked是没有意义的。
func (c *gcControllerState) memoryLimitHeapGoal() uint64 {
	// Start by pulling out some values we'll need. Be careful about overflow.
	var heapFree, heapAlloc, mappedReady uint64
	for {
		heapFree = c.heapFree.load()                         // Free and unscavenged memory.
		heapAlloc = c.totalAlloc.Load() - c.totalFree.Load() // Heap object bytes in use.
		mappedReady = c.mappedReady.Load()                   // Total unreleased mapped memory.
		if heapFree+heapAlloc <= mappedReady {
			// 应该总是成立的，但每个字段是独立更新的。所以需要循环测试直到满足此条件。
			break
		}
		// It is impossible for total unreleased mapped memory to exceed heap memory, but
		// because these stats are updated independently, we may observe a partial update
		// including only some values. Thus, we appear to break the invariant. However,
		// this condition is necessarily transient, so just try again. In the case of a
		// persistent accounting error, we'll deadlock here.
	}

	// Below we compute a goal from memoryLimit. There are a few things to be aware of.
	// Firstly, the memoryLimit does not easily compare to the heap goal: the former
	// is total mapped memory by the runtime that hasn't been released, while the latter is
	// only heap object memory. Intuitively, the way we convert from one to the other is to
	// subtract everything from memoryLimit that both contributes to the memory limit (so,
	// ignore scavenged memory) and doesn't contain heap objects. This isn't quite what
	// lines up with reality, but it's a good starting point.
	//
	// In practice this computation looks like the following:
	//
	//    memoryLimit - ((mappedReady - heapFree - heapAlloc) + max(mappedReady - memoryLimit, 0)) - memoryLimitHeapGoalHeadroom
	//                    ^1                                    ^2                                   ^3
	//
	// Let's break this down.
	//
	// The first term (marker 1) is everything that contributes to the memory limit and isn't
	// or couldn't become heap objects. It represents, broadly speaking, non-heap overheads.
	// One oddity you may have noticed is that we also subtract out heapFree, i.e. unscavenged
	// memory that may contain heap objects in the future.
	//
	// Let's take a step back. In an ideal world, this term would look something like just
	// the heap goal. That is, we "reserve" enough space for the heap to grow to the heap
	// goal, and subtract out everything else. This is of course impossible; the definition
	// is circular! However, this impossible definition contains a key insight: the amount
	// we're *going* to use matters just as much as whatever we're currently using.
	//
	// Consider if the heap shrinks to 1/10th its size, leaving behind lots of free and
	// unscavenged memory. mappedReady - heapAlloc will be quite large, because of that free
	// and unscavenged memory, pushing the goal down significantly.
	//
	// heapFree is also safe to exclude from the memory limit because in the steady-state, it's
	// just a pool of memory for future heap allocations, and making new allocations from heapFree
	// memory doesn't increase overall memory use. In transient states, the scavenger and the
	// allocator actively manage the pool of heapFree memory to maintain the memory limit.
	//
	// The second term (marker 2) is the amount of memory we've exceeded the limit by, and is
	// intended to help recover from such a situation. By pushing the heap goal down, we also
	// push the trigger down, triggering and finishing a GC sooner in order to make room for
	// other memory sources. Note that since we're effectively reducing the heap goal by X bytes,
	// we're actually giving more than X bytes of headroom back, because the heap goal is in
	// terms of heap objects, but it takes more than X bytes (e.g. due to fragmentation) to store
	// X bytes worth of objects.
	//
	// The third term (marker 3) subtracts an additional memoryLimitHeapGoalHeadroom bytes from the
	// heap goal. As the name implies, this is to provide additional headroom in the face of pacing
	// inaccuracies. This is a fixed number of bytes because these inaccuracies disproportionately
	// affect small heaps: as heaps get smaller, the pacer's inputs get fuzzier. Shorter GC cycles
	// and less GC work means noisy external factors like the OS scheduler have a greater impact.

	memoryLimit := uint64(c.memoryLimit.Load())

	// Compute term 1.
	nonHeapMemory := mappedReady - heapFree - heapAlloc

	// Compute term 2.
	var overage uint64
	if mappedReady > memoryLimit {
		overage = mappedReady - memoryLimit
	}

	if nonHeapMemory+overage >= memoryLimit {
		// We're at a point where non-heap memory exceeds the memory limit on its own.
		// There's honestly not much we can do here but just trigger GCs continuously
		// and let the CPU limiter reign that in. Something has to give at this point.
		// Set it to heapMarked, the lowest possible goal.
		return c.heapMarked
	}

	// Compute the goal.
	goal := memoryLimit - (nonHeapMemory + overage)

	// Apply some headroom to the goal to account for pacing inaccuracies.
	// Be careful about small limits.
	if goal < memoryLimitHeapGoalHeadroom || goal-memoryLimitHeapGoalHeadroom < memoryLimitHeapGoalHeadroom {
		goal = memoryLimitHeapGoalHeadroom
	} else {
		goal = goal - memoryLimitHeapGoalHeadroom
	}
	// Don't let us go below the live heap. A heap goal below the live heap doesn't make sense.
	if goal < c.heapMarked {
		goal = c.heapMarked
	}
	return goal
}

const (
	// These constants determine the bounds on the GC trigger as a fraction
	// of heap bytes allocated between the start of a GC (heapLive == heapMarked)
	// and the end of a GC (heapLive == heapGoal).
	//
	// The constants are obscured in this way for efficiency. The denominator
	// of the fraction is always a power-of-two for a quick division, so that
	// the numerator is a single constant integer multiplication.
	triggerRatioDen = 64

	// The minimum trigger constant was chosen empirically: given a sufficiently
	// fast/scalable allocator with 48 Ps that could drive the trigger ratio
	// to <0.05, this constant causes applications to retain the same peak
	// RSS compared to not having this allocator.
	minTriggerRatioNum = 45 // ~0.7

	// The maximum trigger constant is chosen somewhat arbitrarily, but the
	// current constant has served us well over the years.
	maxTriggerRatioNum = 61 // ~0.95
)

// trigger returns the current point at which a GC should trigger along with
// the heap goal.
//
// The returned value may be compared against heapLive to determine whether
// the GC should trigger. Thus, the GC trigger condition should be (but may
// not be, in the case of small movements for efficiency) checked whenever
// the heap goal may change.
//
// trigger(),gcTrigger.test 函数所调用, gcControllerCommit 函数所调用。
// 用于验证 gcTriggerHeap 类型的 gcTrigger 是否满足开始GC的条件。
//
// 返回值：
// *******trigger的值*********
// triggerLowerBound(最小值) := (goal-c.heapMarked)*0.7 + c.heapMarked
// maxTrigger(最大值) := (goal-c.heapMarked)*0.95 + c.heapMarked
// goal - runway[上次GC标记期间分配的字节数(大概)](理想值，让堆到达goal时标记任务刚好能结束)
// ****************
// goal/trigger(在test中触发参照值)
// trigger 大概率trigger是goal-runway，且与goal所差无几。
// minTrigger/_(在test中忽略此返回值)
// minTrigger 大概率为 triggerLowerBound。
//
// minTrigger <= goal
func (c *gcControllerState) trigger() (uint64, uint64) {
	// minTrigger < goal
	goal, minTrigger := c.heapGoalInternal()

	// Invariant: the trigger must always be less than the heap goal.
	//
	// Note that the memory limit sets a hard maximum on our heap goal,
	// but the live heap may grow beyond it.

	if c.heapMarked >= goal {
		// 一般不会出现这种情况。
		//
		// The goal should never be smaller than heapMarked, but let's be
		// defensive about it. The only reasonable trigger here is one that
		// causes a continuous GC cycle at heapMarked, but respect the goal
		// if it came out as smaller than that.
		return goal, goal
	}

	// Below this point, c.heapMarked < goal.

	// heapMarked is our absolute minimum, and it's possible the trigger
	// bound we get from heapGoalinternal is less than that.
	if minTrigger < c.heapMarked {
		// 一般不会出现这种情况。目前观察到只有第一个GC开始之前，minTrigger会是0。
		minTrigger = c.heapMarked
	}

	// If we let the trigger go too low, then if the application
	// is allocating very rapidly we might end up in a situation
	// where we're allocating black during a nearly always-on GC.
	//
	// The result of this is a growing heap and ultimately an
	// increase in RSS. By capping us at a point >0, we're essentially
	// saying that we're OK using more CPU during the GC to prevent
	// this growth in RSS.
	//
	// triggerRationDen = 64
	// minTriggerRatioNum = 45
	triggerLowerBound := uint64(((goal-c.heapMarked)/triggerRatioDen)*minTriggerRatioNum) + c.heapMarked
	if minTrigger < triggerLowerBound {
		// 大概率会发生
		minTrigger = triggerLowerBound
	}

	// For small heaps, set the max trigger point at maxTriggerRatio of the way
	// from the live heap to the heap goal.
	// This ensures we always have *some*
	// headroom when the GC actually starts. For larger heaps, set the max trigger
	// point at the goal, minus the minimum heap size.
	//
	// This choice follows from the fact that the minimum heap size is chosen
	// to reflect the costs of a GC with no work to do. With a large heap but
	// very little scan work to perform, this gives us exactly as much runway
	// as we would need, in the worst case.
	//
	// defaultHeapMinimum: 4194304
	// triggerRatioDen: 64
	// maxTriggerRatioNum: 61; 61/64=0.95
	// 如果GOGC为100堆不大的情况下，则 goal、maxTrigger 所差无几。
	maxTrigger := uint64(((goal-c.heapMarked)/triggerRatioDen)*maxTriggerRatioNum) + c.heapMarked
	if goal > defaultHeapMinimum && goal-defaultHeapMinimum > maxTrigger {
		// 大堆会发生，至少80MB往上。
		maxTrigger = goal - defaultHeapMinimum
	}
	if maxTrigger < minTrigger {
		maxTrigger = minTrigger
	}

	// Compute the trigger from our bounds and the runway stored by commit.
	//
	// 通过 runway 、 miniTrigger 和 maxTrigger 来计算trigger。
	var trigger uint64
	runway := c.runway.Load()
	if runway > goal {
		// 正常GOGC的情况下 runway 不可能大于goal。
		trigger = minTrigger
	} else {
		trigger = goal - runway
	}
	// ****************
	// triggerLowerBound := uint64(((goal-c.heapMarked)/triggerRatioDen)*minTriggerRatioNum) + c.heapMarked
	// maxTrigger := uint64(((goal-c.heapMarked)/triggerRatioDen)*maxTriggerRatioNum) + c.heapMarked
	// goal - runway(理想值，让堆到达goal时标记任务刚好能结束)
	// ****************
	if trigger < minTrigger {
		trigger = minTrigger
	}
	if trigger > maxTrigger {
		trigger = maxTrigger
	}
	if trigger > goal {
		print("trigger=", trigger, " heapGoal=", goal, "\n")
		print("minTrigger=", minTrigger, " maxTrigger=", maxTrigger, "\n")
		throw("produced a trigger greater than the heap goal")
	}
	// trigger=< goal
	//
	// *******trigger的值*********
	// triggerLowerBound(最小值) := (goal-c.heapMarked)*0.7 + c.heapMarked
	// maxTrigger(最大值) := (goal-c.heapMarked)*0.95 + c.heapMarked
	// goal - runway[上次GC标记期间分配的字节数(大概)](理想值，让堆到达goal时标记任务刚好能结束)
	// ****************
	// 大概率trigger是goal-runway，且与goal所差无几。
	return trigger, goal
}

// commit recomputes all pacing parameters needed to derive the
// trigger and the heap goal. Namely, the gcPercent-based heap goal,
// and the amount of runway we want to give the GC this cycle.
//
// This can be called any time. If GC is the in the middle of a
// concurrent phase, it will adjust the pacing of that phase.
//
// isSweepDone should be the result of calling isSweepDone(),
// unless we're testing or we know we're executing during a GC cycle.
//
// This depends on gcPercent, gcController.heapMarked, and
// gcController.heapLive. These must be up to date.
//
// Callers must call gcControllerState.revise after calling this
// function if the GC is enabled.
//
// 被以下函数所调用：
// 1. gcControllerCommit , isSweepDone = false
// 2. gcControllerState.init , isSweepDone = true
//
// mheap_.lock must be held or the world must be stopped.
func (c *gcControllerState) commit(isSweepDone bool) {
	if !c.test {
		assertWorldStoppedOrLockHeld(&mheap_.lock)
	}

	if isSweepDone {
		// The sweep is done, so there aren't any restrictions on the trigger
		// we need to think about.
		c.sweepDistMinTrigger.Store(0)
	} else {
		// Concurrent sweep happens in the heap growth
		// from gcController.heapLive to trigger. Make sure we
		// give the sweeper some runway if it doesn't have enough.
		//
		// 此时 c.heapLive 是GC标记结束后但还未 START THE WORLD，
		// GC标记的字节数。
		// sweepMinHeapDistance = 1024 * 1024
		c.sweepDistMinTrigger.Store(c.heapLive.Load() + sweepMinHeapDistance)
	}

	// Compute the next GC goal, which is when the allocated heap
	// has grown by GOGC/100 over where it started the last cycle,
	// plus additional runway for non-heap sources of GC work.
	gcPercentHeapGoal := ^uint64(0)
	if gcPercent := c.gcPercent.Load(); gcPercent >= 0 {
		gcPercentHeapGoal = c.heapMarked + (c.heapMarked+c.lastStackScan.Load()+c.globalsScan.Load())*uint64(gcPercent)/100
		/*
			println(gcPercent)
			println(c.heapMarked)
			println(c.lastStackScan.Load())
			println(c.globalsScan.Load())
			println(gcPercentHeapGoal)
		*/
	}
	// Apply the minimum heap size here. It's defined in terms of gcPercent
	// and is only updated by functions that call commit.
	//
	// c.heapMinimum 的值为4MB*GOGC/100
	if gcPercentHeapGoal < c.heapMinimum {
		gcPercentHeapGoal = c.heapMinimum
	}
	c.gcPercentHeapGoal.Store(gcPercentHeapGoal)

	// Compute the amount of runway we want the GC to have by using our
	// estimate of the cons/mark ratio.
	//
	// The idea is to take our expected scan work, and multiply it by
	// the cons/mark ratio to determine
	// **how long it'll take to complete**
	// that scan work in terms of bytes allocated. This gives us our GC's
	// runway.
	//
	// However, the cons/mark ratio is a ratio of rates per CPU-second, but
	// here we care about the relative rates for some division of CPU
	// resources among the mutator and the GC.
	//
	// To summarize, we have B / cpu-ns, and we want B / ns. We get that
	// by multiplying by our desired division of CPU resources.
	// We choose
	// to express CPU resources as GOMAPROCS*fraction. Note that because
	// we're working with a ratio here, we can omit the number of CPU cores,
	// because they'll appear in the numerator and denominator and cancel out.
	//
	// As a result, this is basically just "weighing" the cons/mark ratio by
	// our desired division of resources.
	//
	// Furthermore, by setting the runway so that CPU resources are divided
	// this way, assuming that the cons/mark ratio is correct, we make that
	// division a reality.
	c.runway.Store(uint64((c.consMark * (1 - gcGoalUtilization) / (gcGoalUtilization)) * float64(c.lastHeapScan+c.lastStackScan.Load()+c.globalsScan.Load())))
}

// setGCPercent updates gcPercent. commit must be called after.
// Returns the old value of gcPercent.
//
// The world must be stopped, or mheap_.lock must be held.
func (c *gcControllerState) setGCPercent(in int32) int32 {
	if !c.test {
		assertWorldStoppedOrLockHeld(&mheap_.lock)
	}

	out := c.gcPercent.Load()
	if in < 0 {
		in = -1
	}
	c.heapMinimum = defaultHeapMinimum * uint64(in) / 100
	c.gcPercent.Store(in)

	return out
}

//go:linkname setGCPercent runtime/debug.setGCPercent
func setGCPercent(in int32) (out int32) {
	// Run on the system stack since we grab the heap lock.
	systemstack(func() {
		lock(&mheap_.lock)
		out = gcController.setGCPercent(in)
		gcControllerCommit()
		unlock(&mheap_.lock)
	})

	// If we just disabled GC, wait for any concurrent GC mark to
	// finish so we always return with no GC running.
	if in < 0 {
		gcWaitOnMark(work.cycles.Load())
	}

	return out
}

func readGOGC() int32 {
	p := gogetenv("GOGC")
	if p == "off" {
		return -1
	}
	if n, ok := atoi32(p); ok {
		return n
	}
	return 100
}

// setMemoryLimit updates memoryLimit. commit must be called after
// Returns the old value of memoryLimit.
//
// The world must be stopped, or mheap_.lock must be held.
func (c *gcControllerState) setMemoryLimit(in int64) int64 {
	if !c.test {
		assertWorldStoppedOrLockHeld(&mheap_.lock)
	}

	out := c.memoryLimit.Load()
	if in >= 0 {
		c.memoryLimit.Store(in)
	}

	return out
}

//go:linkname setMemoryLimit runtime/debug.setMemoryLimit
func setMemoryLimit(in int64) (out int64) {
	// Run on the system stack since we grab the heap lock.
	systemstack(func() {
		lock(&mheap_.lock)
		out = gcController.setMemoryLimit(in)
		if in < 0 || out == in {
			// If we're just checking the value or not changing
			// it, there's no point in doing the rest.
			unlock(&mheap_.lock)
			return
		}
		gcControllerCommit()
		unlock(&mheap_.lock)
	})
	return out
}

func readGOMEMLIMIT() int64 {
	p := gogetenv("GOMEMLIMIT")
	if p == "" || p == "off" {
		return maxInt64
	}
	n, ok := parseByteCount(p)
	if !ok {
		print("GOMEMLIMIT=", p, "\n")
		throw("malformed GOMEMLIMIT; see `go doc runtime/debug.SetMemoryLimit`")
	}
	return n
}

// addIdleMarkWorker
// 目的:尝试增加 idleWorker 的计数，如果增加成功调用者需称为一个 idleWorker
// 返回值：
//	false，满足下列条件之一:
//		 1.当期正在运行的 idleWorker 已经超过了所允许的最大 idleWorker 的数量
//       2.cas增加idleWorker计数失败
//  true，其它情况
//
// gcControllerState.idleMarkWorkers 高32位的值为 idleWorker允许的最大数量
// 低32位的值为当前 idleWorker的数量
//
// addIdleMarkWorker attempts to add a new idle mark worker.
//
// If this returns true, the caller must become an idle mark worker unless
// there's no background mark worker goroutines in the pool. This case is
// harmless because there are already background mark workers running.
// If this returns false, the caller must NOT become an idle mark worker.
//
// nosplit because it may be called without a P.
//
//go:nosplit
func (c *gcControllerState) addIdleMarkWorker() bool {
	for {
		old := c.idleMarkWorkers.Load()
		n, max := int32(old&uint64(^uint32(0))), int32(old>>32)
		if n >= max {
			// See the comment on idleMarkWorkers for why
			// n > max is tolerated.
			return false
		}
		// n<max 并不代表下面一定可以成功，所以要不断进行cas尝试。
		// 因为允许并发操作表示idleWroker现存数量的低32位。
		if n < 0 {
			print("n=", n, " max=", max, "\n")
			throw("negative idle mark workers")
		}
		new := uint64(uint32(n+1)) | (uint64(max) << 32)
		if c.idleMarkWorkers.CompareAndSwap(old, new) {
			return true
		}
	}
}

// needIdleMarkWorker is a hint as to whether another idle mark worker is needed.
//
// The caller must still call addIdleMarkWorker to become one. This is mainly
// useful for a quick check before an expensive operation.
//
// nosplit because it may be called without a P.
//
// 一个可以立刻知道现在是否需要ideMarkWOrker(现存数量小于允许最大数量)的fast path函数。
// 但返回true并不是说接下来尝试增加一个idleMarkWorker会成功。
//
//go:nosplit
func (c *gcControllerState) needIdleMarkWorker() bool {
	p := c.idleMarkWorkers.Load()
	n, max := int32(p&uint64(^uint32(0))), int32(p>>32)
	return n < max
}

// removeIdleMarkWorker must be called when an new idle mark worker stops executing.
//
// 肯定可以成功，因为最低32位(现存idleMarkWorker数量)是谁负责增加谁就负责减少。
// 所以不会负数情况。
func (c *gcControllerState) removeIdleMarkWorker() {
	for {
		old := c.idleMarkWorkers.Load()
		n, max := int32(old&uint64(^uint32(0))), int32(old>>32)
		if n-1 < 0 {
			print("n=", n, " max=", max, "\n")
			throw("negative idle mark workers")
		}
		new := uint64(uint32(n-1)) | (uint64(max) << 32)
		if c.idleMarkWorkers.CompareAndSwap(old, new) {
			return
		}
	}
}

// setMaxIdleMarkWorkers sets the maximum number of idle mark workers allowed.
//
// This method is optimistic in that it does not wait for the number of
// idle mark workers to reduce to max before returning; it assumes the workers
// will deschedule themselves.
//
// 将max存储到高32位表示MaxIdleMarkWorkers，同时保留现存的idleMarkWorker的数量不受影响。
func (c *gcControllerState) setMaxIdleMarkWorkers(max int32) {
	for {
		old := c.idleMarkWorkers.Load()
		n := int32(old & uint64(^uint32(0)))
		if n < 0 {
			print("n=", n, " max=", max, "\n")
			throw("negative idle mark workers")
		}
		new := uint64(uint32(n)) | (uint64(max) << 32)
		if c.idleMarkWorkers.CompareAndSwap(old, new) {
			return
		}
	}
}

// gcControllerCommit is gcController.commit, but passes arguments from live
// (non-test) data. It also updates any consumers of the GC pacing, such as
// sweep pacing and the background scavenger.
//
// Calls gcController.commit.
//
// The heap lock must be held, so this must be executed on the system stack.
//
// 位于 resetLive 函数之后调用
// gcControllerCommit 函数被 setGCPercent 、 setMemoryLimit 和 gcMarkTermination
//
// 正常只会被 gcMarkTermination 调用。
// 具体时机：
// 在刚刚调用 setGCPhase(_GCoff) 之后。
// -----------
// 更新gcController的一些统计字段，作为下一轮GC触发的依据。
// 根据上一轮GC的goal和本轮标记结束时的goal更新 scavenge 的一些字段。作为是否进行scavenge的依据。
// 根据 gcController.trigger() 的返回值，指导 sweep 的步伐。
//
// 即除了GC本身一些统计数据的更新，同时也利用这些统计数据来更新scavenge和sweep在本轮GC[从此函数被调用到下一次被调用的这段期间]的执行节奏。
//
//go:systemstack
func gcControllerCommit() {
	assertWorldStoppedOrLockHeld(&mheap_.lock)

	gcController.commit(isSweepDone())

	// Update mark pacing.
	if gcphase != _GCoff {
		// 被gcMarkTermination不会到这里。
		gcController.revise()
	}

	// TODO(mknyszek): This isn't really accurate any longer because the heap
	// goal is computed dynamically. Still useful to snapshot, but not as useful.
	if trace.enabled {
		traceHeapGoal()
	}

	// 下一轮GC触发目标
	trigger, heapGoal := gcController.trigger()
	gcPaceSweeper(trigger)

	gcPaceScavenger(gcController.memoryLimit.Load(), heapGoal, gcController.lastHeapGoal)
}
