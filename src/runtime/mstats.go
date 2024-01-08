// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Memory statistics

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

type mstats struct {
	// Statistics about malloc heap.
	heapStats consistentHeapStats

	// Statistics about stacks.
	stacks_sys sysMemStat // only counts newosproc0 stack in mstats; differs from MemStats.StackSys

	// Statistics about allocation of low-level fixed-size structures.
	mspan_sys    sysMemStat
	mcache_sys   sysMemStat
	buckhash_sys sysMemStat // profiling bucket hash table

	// Statistics about GC overhead.
	// heapArena 类型值的分配
	// mheap.allArenas
	gcMiscSys sysMemStat // updated atomically or during STW

	// Miscellaneous statistics.
	other_sys sysMemStat // updated atomically or during STW

	// Statistics about the garbage collector.

	// Protected by mheap or stopping the world during GC.
	last_gc_unix    uint64 // last gc (in unix time)
	pause_total_ns  uint64
	// 清扫终止阶段中从stopTheWorld调用之前到startTheWorld调用之后这段时间+
	// +
	// 标记终止阶段从stopTheWorld调用之前到在gcTermination中将GC设置为_GCoff之后。
	// 第n轮GC对应pause_ns[n+255%256]
	pause_ns        [256]uint64 // circular buffer of recent gc pause lengths
	// 标记终止阶段GC此时已经设置到_GCoff状态，还未star the world.
	// 取这个时候的now时间。第n轮GC对应pause_end[n+255%256]
	pause_end       [256]uint64 // circular buffer of recent gc end times (nanoseconds since 1970)
	// 从1开始，表示第几轮GC。
	// gcMarkTermination 中设置，start the world 调用之前。
	numgc           uint32
	// runtime.GC 进行GC的计数。
	// 只增加。
	numforcedgc     uint32  // number of user-forced GCs
	gc_cpu_fraction float64 // fraction of CPU time used by GC

	last_gc_nanotime uint64 // last gc (monotonic time)
    // 等于标记终止阶段 gcController.heapInUse 的值，在 gcMarkTermination 函数中设置。
	lastHeapInUse    uint64 // heapInUse at mark termination of the previous GC

	enablegc bool

	// gcPauseDist represents the distribution of all GC-related
	// application pauses in the runtime.
	//
	// Each individual pause is counted separately, unlike pause_ns.
	gcPauseDist timeHistogram
}

var memstats mstats

// A MemStats records statistics about the memory allocator.
type MemStats struct {
	// General statistics.

	// Alloc is bytes of allocated heap objects.
	//
	// This is the same as HeapAlloc (see below).
	// 所有可达对象及不可达但未被清扫的对象的大小总和。
	Alloc uint64

	// TotalAlloc is cumulative bytes allocated for heap objects.
	//
	// TotalAlloc increases as heap objects are allocated, but
	// unlike Alloc and HeapAlloc, it does not decrease when
	// objects are freed.
	//
	// memstats.heapStats.stats里的所有heapStatsDelta的smallAllocCount+ largeAllocCount 的结果。
	TotalAlloc uint64

	// Sys is the total bytes of memory obtained from the OS.
	//
	// Sys is the sum of the XSys fields below. Sys measures the
	// virtual address space reserved by the Go runtime for the
	// heap, stacks, and other internal data structures. It's
	// likely that not all of the virtual address space is backed
	// by physical memory at any given moment, though in general
	// it all was at some point.
	//
	// 从操作系统申请的虚拟地址空间总大小（处于ready状态的地址空间）。
	//
	// 	totalMapped := gcController.heapInUse.load() + gcController.heapFree.load() + gcController.heapReleased.load() +
	//		memstats.stacks_sys.load() + memstats.mspan_sys.load() + memstats.mcache_sys.load() +
	//		memstats.buckhash_sys.load() + memstats.gcMiscSys.load() + memstats.other_sys.load() +
	//		stackInUse + gcWorkBufInUse + gcProgPtrScalarBitsInUse
	Sys uint64

	// Lookups is the number of pointer lookups performed by the
	// runtime.
	//
	// This is primarily useful for debugging runtime internals.
	Lookups uint64

	// Mallocs is the cumulative count of heap objects allocated.
	// The number of live objects is Mallocs - Frees.
	//
	// 已分配对象的个数（只增不减）。
	// memstats.heapStats.stats里的所有heapStatsDelta的largeAllocCount + tinyAllocCount 的结果。
	Mallocs uint64

	// Frees is the cumulative count of heap objects freed.
	//
	// 已经sweept的对象个数（只增不减）。
	// memstats.heapStats.stats里的所有heapStatsDelta的largeFreeCount + tinyAllocCount 的结果。
	Frees uint64

	// Heap memory statistics.
	//
	// Interpreting the heap statistics requires some knowledge of
	// how Go organizes memory. Go divides the virtual address
	// space of the heap into "spans", which are contiguous
	// regions of memory 8K or larger. A span may be in one of
	// three states:
	//
	// An "idle" span contains no objects or other data. The
	// physical memory backing an idle span can be released back
	// to the OS (but the virtual address space never is), or it
	// can be converted into an "in use" or "stack" span.
	//
	// An "in use" span contains at least one heap object and may
	// have free space available to allocate more heap objects.
	//
	// A "stack" span is used for goroutine stacks. Stack spans
	// are not considered part of the heap. A span can change
	// between heap and stack memory; it is never used for both
	// simultaneously.

	// HeapAlloc is bytes of allocated heap objects.
	//
	// "Allocated" heap objects include all reachable objects, as
	// well as unreachable objects that the garbage collector has
	// not yet freed. Specifically, HeapAlloc increases as heap
	// objects are allocated and decreases as the heap is swept
	// and unreachable objects are freed. Sweeping occurs
	// incrementally between GC cycles, so these two processes
	// occur simultaneously, and as a result HeapAlloc tends to
	// change smoothly (in contrast with the sawtooth that is
	// typical of stop-the-world garbage collectors).
	//
	// 所有mspan总的可达对象和不可达但还没有被清扫的对象的大小总和
	//
	// 此字段直接读取没有意义，因为此字段统计的信息除了实时更新的
	// memstats.heapStats.largeAlloc，
	// 其它都来自于所有p缓存的mspan中从赋予p之后被使用的对象的总字节数。
	//
	// 该字段主要用于调试目的，runtime并不会主动更新此此字段。
	// largeAlloc + smallAllocCount - largeFree - smallFreeCount 的结果。
	HeapAlloc uint64

	// HeapSys is bytes of heap memory obtained from the OS.
	//
	// HeapSys measures the amount of virtual address space
	// reserved for the heap. This includes virtual address space
	// that has been reserved but not yet used, which consumes no
	// physical memory, but tends to be small, as well as virtual
	// address space for which the physical memory has been
	// returned to the OS after it became unused (see HeapReleased
	// for a measure of the latter).
	//
	// HeapSys estimates the largest size the heap has had.
	//
	// 为堆内存申请的虚拟地址空间大小（ready状态的地址空间）。
	// gcController.heapInUse.load() + gcController.heapFree.load() + gcController.heapReleased.load()
	HeapSys uint64

	// HeapIdle is bytes in idle (unused) spans.
	//
	// Idle spans have no objects in them. These spans could be
	// (and may already have been) returned to the OS, or they can
	// be reused for heap allocations, or they can be reused as
	// stack memory.
	//
	// HeapIdle minus HeapReleased estimates the amount of memory
	// that could be returned to the OS, but is being retained by
	// the runtime so it can grow the heap without requesting more
	// memory from the OS. If this difference is significantly
	// larger than the heap size, it indicates there was a recent
	// transient spike in live heap size.
	//
	// stats.HeapIdle = gcController.heapFree.load() + gcController.heapReleased.load()
	// 页面对应的 pageAlloc.chunks 的scavenged位是1。heapReleased
	// +
	// 页面对应的 pageAlloc.chunks 的scavenged位是0，alloc位也为0。heapFree
	HeapIdle uint64

	// HeapInuse is bytes in in-use spans.
	//
	// In-use spans have at least one object in them. These spans
	// can only be used for other objects of roughly the same
	// size.
	//
	// HeapInuse minus HeapAlloc estimates the amount of memory
	// that has been dedicated to particular size classes, but is
	// not currently being used. This is an upper bound on
	// fragmentation, but in general this memory can be reused
	// efficiently.
	//
	// spanAllocHeap 类型的msapn，在 mheap.grow 方法中增加，在 mheap.freeSpanLocked 中
	// 减少。这类型的所有msapn管理的内存大小总和。
	// 页对应的分配位是1。
	// = gcController.heapInUse
	HeapInuse uint64

	// HeapReleased is bytes of physical memory returned to the OS.
	//
	// This counts heap memory from idle spans that was returned
	// to the OS and has not yet been reacquired for the heap.
	//
	// 所有scavenge位为1的页的大小总和。
	// = gcController.heapReleased
	HeapReleased uint64

	// HeapObjects is the number of allocated heap objects.
	//
	// Like HeapAlloc, this increases as objects are allocated and
	// decreases as the heap is swept and unreachable objects are
	// freed.
	//
	// 可达和不可达但未清扫的对象个数总和。
	// largeAllocCount + smallAllocCount
	HeapObjects uint64

	// Stack memory statistics.
	//
	// Stacks are not considered part of the heap, but the runtime
	// can reuse a span of heap memory for stack memory, and
	// vice-versa.

	// StackInuse is bytes in stack spans.
	//
	// In-use stack spans have at least one stack in them. These
	// spans can only be used for other stacks of the same size.
	//
	// There is no StackIdle because unused stack spans are
	// returned to the heap (and hence counted toward HeapIdle).
	//
	// 那些服务于stack的span的大小总和。
	//
	// 在 mheap.allocSpan 中增加此计数。
	// 只统计 spanAllocStack 类型的mspan在分配其管理内存时需要的页的总字节大小。
	// 在 mheap.freeSpanLocked 中减少此计数。
	// 只统计 spanAllocStack 类型的msapn在将其底层对应的页(页对应的分配位由1变成0)释放到页堆时，减少这些页的总字节数。
	// inStacks        int64 // byte delta of memory reserved for stacks
	StackInuse uint64

	// StackSys is bytes of stack memory obtained from the OS.
	//
	// StackSys is StackInuse, plus any memory obtained directly
	// from the OS for OS thread stacks (which should be minimal).
	//
	// 在 mheap.allocSpan 中增加此计数。
	// 只统计 spanAllocStack 类型的mspan在分配其管理内存时需要的页的总字节大小。
	// 在 mheap.freeSpanLocked 中减少此计数。
	// 只统计 spanAllocStack 类型的msapn在将其底层对应的页(页对应的分配位由1变成0)释放到页堆时，减少这些页的总字节数。
	// inStacks        int64 // byte delta of memory reserved for stacks
	// 上面的(inStacks) + 有些g0使用系统分配的栈的大小。
	StackSys uint64

	// Off-heap memory statistics.
	//
	// The following statistics measure runtime-internal
	// structures that are not allocated from heap memory (usually
	// because they are part of implementing the heap). Unlike
	// heap or stack memory, any memory allocated to these
	// structures is dedicated to these structures.
	//
	// These are primarily useful for debugging runtime memory
	// overheads.

	// MSpanInuse is bytes of allocated mspan structures.
	//
	// 供mspan结构体本身使用的内存大小(正在使用的mspan)。
	MSpanInuse uint64

	// MSpanSys is bytes of memory obtained from the OS for mspan
	// structures.
	//
	// 从操作系统申请的用于分配mspan结构体的内存的大小。
	MSpanSys uint64

	// MCacheInuse is bytes of allocated mcache structures.
	//
	// 供mcache结构体本身使用的内存大小(正在使用的mcache)。
	MCacheInuse uint64

	// MCacheSys is bytes of memory obtained from the OS for
	// mcache structures.
	//
	// 从操作系统申请的用于分配mcache结构体的内存的大小。
	MCacheSys uint64

	// BuckHashSys is bytes of memory in profiling bucket hash tables.
	// 供 bucket 结构体本身使用的内存大小。
	BuckHashSys uint64

	// GCSys is bytes of memory in garbage collection metadata.
	//
	// 从操作系统申请供GC使用的内存大小。
	GCSys uint64

	// OtherSys is bytes of memory in miscellaneous off-heap
	// runtime allocations.
	// 系统非堆内存分配总大小，主要是一些维护堆的元数据信息的一些结构体类型所占的内存。
	OtherSys uint64

	// Garbage collector statistics.

	// NextGC is the target heap size of the next GC cycle.
	//
	// The garbage collector's goal is to keep HeapAlloc ≤ NextGC.
	// At the end of each GC cycle, the target for the next cycle
	// is computed based on the amount of reachable data and the
	// value of GOGC.
	//
	// 本轮GC标记终止时gcController.heapGoal()的值，是下一轮GC触发时的比对值。
	NextGC uint64

	// LastGC is the time the last garbage collection finished, as
	// nanoseconds since 1970 (the UNIX epoch).
	// 标记刚刚完成，还没有START THE WORLD。
	// 在函数 gcMarkTermination 中被设置。
	// ----
	// 上一轮GC标记结束时 gcController.heapGoal()的值。
	LastGC uint64

	// PauseTotalNs is the cumulative nanoseconds in GC
	// stop-the-world pauses since the program started.
	//
	// During a stop-the-world pause, all goroutines are paused
	// and only the garbage collector can run.
	//
	// 累计的 PauseNs。
	// 不会减少。
	PauseTotalNs uint64

	// PauseNs is a circular buffer of recent GC stop-the-world
	// pause times in nanoseconds.
	//
	// The most recent pause is at PauseNs[(NumGC+255)%256]. In
	// general, PauseNs[N%256] records the time paused in the most
	// recent N%256th GC cycle[从1开开始]. There may be multiple pauses per
	// GC cycle; this is the sum of all pauses during a cycle.
	//
	// 清扫终止阶段中从stopTheWorld调用之前到startTheWorld调用之后这段时间+
	// +
	// 标记终止阶段从stopTheWorld调用之前到在gcTermination中将GC设置为_GCoff之后。
	// 第n轮GC对应pause_ns[n+255%256]
	PauseNs [256]uint64

	// PauseEnd is a circular buffer of recent GC pause end times,
	// as nanoseconds since 1970 (the UNIX epoch).
	//
	// This buffer is filled the same way as PauseNs. There may be
	// multiple pauses per GC cycle; this records the end of the
	// last pause in a cycle.
	//
	// 标记终止阶段GC此时已经设置到_GCoff状态，还未star the world.
	// 取这个时候的now时间。第n轮GC对应pause_end[n+255%256]
	PauseEnd [256]uint64

	// NumGC is the number of completed GC cycles.
	//
	// 从1开始，表示第几轮GC
	NumGC uint32

	// NumForcedGC is the number of GC cycles that were forced by
	// the application calling the GC function.
	//
	// 调用 runtime.GC() 进行GC的累加次数。
	NumForcedGC uint32

	// GCCPUFraction is the fraction of this program's available
	// CPU time used by the GC since the program started.
	//
	// GCCPUFraction is expressed as a number between 0 and 1,
	// where 0 means GC has consumed none of this program's CPU. A
	// program's available CPU time is defined as the integral of
	// GOMAXPROCS since the program started. That is, if
	// GOMAXPROCS is 2 and a program has been running for 10
	// seconds, its "available CPU" is 20 seconds. GCCPUFraction
	// does not include CPU time used for write barrier activity.
	//
	// This is the same as the fraction of CPU reported by
	// GODEBUG=gctrace=1.
	//
	// 从程序运行开始，所有GC占用时间的比例，总时间为 GCMAXPROCS*(now-start)。
	GCCPUFraction float64

	// EnableGC indicates that GC is enabled. It is always true,
	// even if GOGC=off.
	// 总是为true。
	EnableGC bool

	// DebugGC is currently unused.
	// 此字段未使用。
	DebugGC bool

	// BySize reports per-size class allocation statistics.
	//
	// BySize[N] gives statistics for allocations of size S where
	// BySize[N-1].Size < S ≤ BySize[N].Size.
	//
	// This does not report allocations larger than BySize[60].Size.
	//
	// 记录sizeClass[0-60]范围对应的mspan中对象的分配和释放信息。
	// 最大记录18KB大小的obj分配和释放信息。
	BySize [61]struct {
		// Size is the maximum byte size of an object in this
		// size class.
		//
		// mspan的elemSize字段的值。
		Size uint32

		// Mallocs is the cumulative count of heap objects
		// allocated in this size class. The cumulative bytes
		// of allocation is Size*Mallocs. The number of live
		// objects in this size class is Mallocs - Frees.
		//
		// 只增不减
		Mallocs uint64

		// Frees is the cumulative count of heap objects freed
		// in this size class.
		//
		// 只增不减
		Frees uint64
	}
}

func init() {
	if offset := unsafe.Offsetof(memstats.heapStats); offset%8 != 0 {
		// println(offset)
		throw("memstats.heapStats not aligned to 8 bytes")
	}
	// Ensure the size of heapStatsDelta causes adjacent fields/slots (e.g.
	// [3]heapStatsDelta) to be 8-byte aligned.
	if size := unsafe.Sizeof(heapStatsDelta{}); size%8 != 0 {
		// println(size)
		throw("heapStatsDelta not a multiple of 8 bytes in size")
	}
}

// ReadMemStats populates m with memory allocator statistics.
//
// The returned memory allocator statistics are up to date as of the
// call to ReadMemStats. This is in contrast with a heap profile,
// which is a snapshot as of the most recently completed garbage
// collection cycle.
func ReadMemStats(m *MemStats) {
	stopTheWorld("read mem stats")

	systemstack(func() {
		readmemstats_m(m)
	})

	startTheWorld()
}

// readmemstats_m populates stats for internal runtime values.
//
// The world must be stopped.
func readmemstats_m(stats *MemStats) {
	assertWorldStopped()

	// Flush mcaches to mcentral before doing anything else.
	//
	// Flushing to the mcentral may in general cause stats to
	// change as mcentral data structures are manipulated.
	systemstack(flushallmcaches)

	// Calculate memory allocator stats.
	// During program execution we only count number of frees and amount of freed memory.
	// Current number of alive objects in the heap and amount of alive heap memory
	// are calculated by scanning all spans.
	// Total number of mallocs is calculated as number of frees plus number of alive objects.
	// Similarly, total amount of allocated memory is calculated as amount of freed memory
	// plus amount of alive heap memory.

	// Collect consistent stats, which are the source-of-truth in some cases.
	var consStats heapStatsDelta
	memstats.heapStats.unsafeRead(&consStats)

	// Collect large allocation stats.
	totalAlloc := consStats.largeAlloc
	nMalloc := consStats.largeAllocCount
	totalFree := consStats.largeFree
	nFree := consStats.largeFreeCount

	// Collect per-sizeclass stats.
	var bySize [_NumSizeClasses]struct {
		Size    uint32
		Mallocs uint64
		Frees   uint64
	}
	for i := range bySize {
		bySize[i].Size = uint32(class_to_size[i])

		// Malloc stats.
		a := consStats.smallAllocCount[i]
		totalAlloc += a * uint64(class_to_size[i])
		nMalloc += a
		bySize[i].Mallocs = a

		// Free stats.
		f := consStats.smallFreeCount[i]
		totalFree += f * uint64(class_to_size[i])
		nFree += f
		bySize[i].Frees = f
	}

	// Account for tiny allocations.
	// For historical reasons, MemStats includes tiny allocations
	// in both the total free and total alloc count. This double-counts
	// memory in some sense because their tiny allocation block is also
	// counted. Tracking the lifetime of individual tiny allocations is
	// currently not done because it would be too expensive.
	nFree += consStats.tinyAllocCount
	nMalloc += consStats.tinyAllocCount

	// Calculate derived stats.

	stackInUse := uint64(consStats.inStacks)
	gcWorkBufInUse := uint64(consStats.inWorkBufs)
	gcProgPtrScalarBitsInUse := uint64(consStats.inPtrScalarBits)

	totalMapped := gcController.heapInUse.load() + gcController.heapFree.load() + gcController.heapReleased.load() +
		memstats.stacks_sys.load() + memstats.mspan_sys.load() + memstats.mcache_sys.load() +
		memstats.buckhash_sys.load() + memstats.gcMiscSys.load() + memstats.other_sys.load() +
		stackInUse + gcWorkBufInUse + gcProgPtrScalarBitsInUse

	heapGoal := gcController.heapGoal()

	// The world is stopped, so the consistent stats (after aggregation)
	// should be identical to some combination of memstats. In particular:
	//
	// * memstats.heapInUse == inHeap
	// * memstats.heapReleased == released
	// * memstats.heapInUse + memstats.heapFree == committed - inStacks - inWorkBufs - inPtrScalarBits
	// * memstats.totalAlloc == totalAlloc
	// * memstats.totalFree == totalFree
	//
	// Check if that's actually true.
	//
	// TODO(mknyszek): Maybe don't throw here. It would be bad if a
	// bug in otherwise benign accounting caused the whole application
	// to crash.
	if gcController.heapInUse.load() != uint64(consStats.inHeap) {
		print("runtime: heapInUse=", gcController.heapInUse.load(), "\n")
		print("runtime: consistent value=", consStats.inHeap, "\n")
		throw("heapInUse and consistent stats are not equal")
	}
	if gcController.heapReleased.load() != uint64(consStats.released) {
		print("runtime: heapReleased=", gcController.heapReleased.load(), "\n")
		print("runtime: consistent value=", consStats.released, "\n")
		throw("heapReleased and consistent stats are not equal")
	}
	heapRetained := gcController.heapInUse.load() + gcController.heapFree.load()
	consRetained := uint64(consStats.committed - consStats.inStacks - consStats.inWorkBufs - consStats.inPtrScalarBits)
	if heapRetained != consRetained {
		print("runtime: global value=", heapRetained, "\n")
		print("runtime: consistent value=", consRetained, "\n")
		throw("measures of the retained heap are not equal")
	}
	if gcController.totalAlloc.Load() != totalAlloc {
		print("runtime: totalAlloc=", gcController.totalAlloc.Load(), "\n")
		print("runtime: consistent value=", totalAlloc, "\n")
		throw("totalAlloc and consistent stats are not equal")
	}
	if gcController.totalFree.Load() != totalFree {
		print("runtime: totalFree=", gcController.totalFree.Load(), "\n")
		print("runtime: consistent value=", totalFree, "\n")
		throw("totalFree and consistent stats are not equal")
	}
	// Also check that mappedReady lines up with totalMapped - released.
	// This isn't really the same type of "make sure consistent stats line up" situation,
	// but this is an opportune time to check.
	if gcController.mappedReady.Load() != totalMapped-uint64(consStats.released) {
		print("runtime: mappedReady=", gcController.mappedReady.Load(), "\n")
		print("runtime: totalMapped=", totalMapped, "\n")
		print("runtime: released=", uint64(consStats.released), "\n")
		print("runtime: totalMapped-released=", totalMapped-uint64(consStats.released), "\n")
		throw("mappedReady and other memstats are not equal")
	}

	// We've calculated all the values we need. Now, populate stats.

	stats.Alloc = totalAlloc - totalFree
	stats.TotalAlloc = totalAlloc
	stats.Sys = totalMapped
	stats.Mallocs = nMalloc
	stats.Frees = nFree
	stats.HeapAlloc = totalAlloc - totalFree
	stats.HeapSys = gcController.heapInUse.load() + gcController.heapFree.load() + gcController.heapReleased.load()
	// By definition, HeapIdle is memory that was mapped
	// for the heap but is not currently used to hold heap
	// objects. It also specifically is memory that can be
	// used for other purposes, like stacks, but this memory
	// is subtracted out of HeapSys before it makes that
	// transition. Put another way:
	//
	// HeapSys = bytes allocated from the OS for the heap - bytes ultimately used for non-heap purposes
	// HeapIdle = bytes allocated from the OS for the heap - bytes ultimately used for any purpose
	//
	// or
	//
	// HeapSys = sys - stacks_inuse - gcWorkBufInUse - gcProgPtrScalarBitsInUse
	// HeapIdle = sys - stacks_inuse - gcWorkBufInUse - gcProgPtrScalarBitsInUse - heapInUse
	//
	// => HeapIdle = HeapSys - heapInUse = heapFree + heapReleased
	stats.HeapIdle = gcController.heapFree.load() + gcController.heapReleased.load()
	stats.HeapInuse = gcController.heapInUse.load()
	stats.HeapReleased = gcController.heapReleased.load()
	stats.HeapObjects = nMalloc - nFree
	stats.StackInuse = stackInUse
	// memstats.stacks_sys is only memory mapped directly for OS stacks.
	// Add in heap-allocated stack memory for user consumption.
	stats.StackSys = stackInUse + memstats.stacks_sys.load()
	stats.MSpanInuse = uint64(mheap_.spanalloc.inuse)
	stats.MSpanSys = memstats.mspan_sys.load()
	stats.MCacheInuse = uint64(mheap_.cachealloc.inuse)
	stats.MCacheSys = memstats.mcache_sys.load()
	stats.BuckHashSys = memstats.buckhash_sys.load()
	// MemStats defines GCSys as an aggregate of all memory related
	// to the memory management system, but we track this memory
	// at a more granular level in the runtime.
	stats.GCSys = memstats.gcMiscSys.load() + gcWorkBufInUse + gcProgPtrScalarBitsInUse
	stats.OtherSys = memstats.other_sys.load()
	stats.NextGC = heapGoal
	stats.LastGC = memstats.last_gc_unix
	stats.PauseTotalNs = memstats.pause_total_ns
	stats.PauseNs = memstats.pause_ns
	stats.PauseEnd = memstats.pause_end
	stats.NumGC = memstats.numgc
	stats.NumForcedGC = memstats.numforcedgc
	stats.GCCPUFraction = memstats.gc_cpu_fraction
	stats.EnableGC = true

	// stats.BySize and bySize might not match in length.
	// That's OK, stats.BySize cannot change due to backwards
	// compatibility issues. copy will copy the minimum amount
	// of values between the two of them.
	copy(stats.BySize[:], bySize[:])
}

//go:linkname readGCStats runtime/debug.readGCStats
func readGCStats(pauses *[]uint64) {
	systemstack(func() {
		readGCStats_m(pauses)
	})
}

// readGCStats_m must be called on the system stack because it acquires the heap
// lock. See mheap for details.
//
//go:systemstack
func readGCStats_m(pauses *[]uint64) {
	p := *pauses
	// Calling code in runtime/debug should make the slice large enough.
	if cap(p) < len(memstats.pause_ns)+3 {
		throw("short slice passed to readGCStats")
	}

	// Pass back: pauses, pause ends, last gc (absolute time), number of gc, total pause ns.
	lock(&mheap_.lock)

	n := memstats.numgc
	if n > uint32(len(memstats.pause_ns)) {
		n = uint32(len(memstats.pause_ns))
	}

	// The pause buffer is circular. The most recent pause is at
	// pause_ns[(numgc-1)%len(pause_ns)], and then backward
	// from there to go back farther in time. We deliver the times
	// most recent first (in p[0]).
	p = p[:cap(p)]
	for i := uint32(0); i < n; i++ {
		j := (memstats.numgc - 1 - i) % uint32(len(memstats.pause_ns))
		p[i] = memstats.pause_ns[j]
		p[n+i] = memstats.pause_end[j]
	}

	p[n+n] = memstats.last_gc_unix
	p[n+n+1] = uint64(memstats.numgc)
	p[n+n+2] = memstats.pause_total_ns
	unlock(&mheap_.lock)
	*pauses = p[:n+n+3]
}

// flushmcache flushes the mcache of allp[i].
//
// The world must be stopped.
//
//go:nowritebarrier
func flushmcache(i int) {
	assertWorldStopped()

	p := allp[i]
	c := p.mcache
	if c == nil {
		return
	}
	c.releaseAll()
	stackcache_clear(c)
}

// flushallmcaches flushes the mcaches of all Ps.
//
// The world must be stopped.
//
//go:nowritebarrier
func flushallmcaches() {
	assertWorldStopped()

	for i := 0; i < int(gomaxprocs); i++ {
		flushmcache(i)
	}
}

// sysMemStat represents a global system statistic that is managed atomically.
//
// This type must structurally be a uint64 so that mstats aligns with MemStats.
type sysMemStat uint64

// load atomically reads the value of the stat.
//
// Must be nosplit as it is called in runtime initialization, e.g. newosproc0.
//
//go:nosplit
func (s *sysMemStat) load() uint64 {
	return atomic.Load64((*uint64)(s))
}

// add atomically adds the sysMemStat by n.
//
// Must be nosplit as it is called in runtime initialization, e.g. newosproc0.
//
//go:nosplit
func (s *sysMemStat) add(n int64) {
	val := atomic.Xadd64((*uint64)(s), n)
	if (n > 0 && int64(val) < n) || (n < 0 && int64(val)+n < n) {
		print("runtime: val=", val, " n=", n, "\n")
		throw("sysMemStat overflow")
	}
}

// heapStatsDelta contains deltas of various runtime memory statistics
// that need to be updated together in order for them to be kept
// consistent with one another.
type heapStatsDelta struct {
	// Memory stats.
	//
	// 在 mheap.allocSpan 方法中增加计数，在 pageAlloc.scavengeOne 方法中减少计数。
	// 页的scavenge位由1变成0增加计数，由0变成1减少计数。计数量为 npages*pageSize。
	committed       int64 // byte delta of memory committed
	//
	// 增加计数：
	// 1. 在 pageAlloc.scavengeOne 中增加计数，被scavenge的总页数的总大小。
	// 2. 在 mheap.grow 中当前的 mheap.curArena 剩余空间不够分配时会将剩余空间全部标记为prepared状态，用剩余经济增加此计数。
	// 因为为了为了满足所需内存的大小，必须重新分配新的arena，就的arena必须更新为可使用状态(prepared剩余地址区间，并将其插入
	// pageAlloc.inUse 中）。
	// 3. 在 mheap.grow 当前/新分配的 mheap.curArena 剩余空间满足此次分配需要。减少此计数，减少量是
	// 因此次分配内存导致 mheap.curArena 的新base与旧base的差值，因为需要对齐，不能简单的认为是请求内存的大小。
	// 减少计数：
	// 2. mheap.allocSpan 中减少计数。从页堆为msapn分配其管理的内存时，这些页中对应的scavenge位为1的页数的总大小来减少此计数。
	// --------
	// 总结：页对应的scavenge位由1变成0减少此计数，新分配的页初始时为1增加此计数，由0变成1时也会增加此计数。
	released        int64 // byte delta of released memory generated
	//
	// 在 mheap.allocSpan 中增加此计数。
	// 只统计 spanAllocHeap 类型的mspan在分配其管理内存时需要的页的总字节大小。
	// 在 mheap.freeSpanLocked 中减少此计数。
	// 只统计 spanAllocHeap 类型的msapn在将其底层对应的页(页对应的分配位由1变成0)释放到页堆时，减少这些页的总字节数。
	inHeap          int64 // byte delta of memory placed in the heap
	// 在 mheap.allocSpan 中增加此计数。
	// 只统计 spanAllocStack 类型的mspan在分配其管理内存时需要的页的总字节大小。
	// 在 mheap.freeSpanLocked 中减少此计数。
	// 只统计 spanAllocStack 类型的msapn在将其底层对应的页(页对应的分配位由1变成0)释放到页堆时，减少这些页的总字节数。
	inStacks        int64 // byte delta of memory reserved for stacks
	// 在 mheap.allocSpan 中增加此计数。
	// 只统计 spanAllocWorkBuf  类型的mspan在分配其管理内存时需要的页的总字节大小。
	// 在 mheap.freeSpanLocked 中减少此计数。
	// 只统计 spanAllocWorkBuf 类型的msapn在将其底层对应的页(页对应的分配位由1变成0)释放到页堆时，减少这些页的总字节数。
	inWorkBufs      int64 // byte delta of memory reserved for work bufs
	// 在 mheap.allocSpan 中增加此计数。
	// 只统计 spanAllocPtrScalarBits 类型的mspan在分配其管理内存时需要的页的总字节大小。
	// 在 mheap.freeSpanLocked 中减少此计数。
	// 只统计 spanAllocPtrScalarBits 类型的msapn在将其底层对应的页(页对应的分配位由1变成0)释放到页堆时，减少这些页的总字节数。
	inPtrScalarBits int64 // byte delta of memory reserved for unrolled GC prog bits

	// Allocator stats.
	//
	// These are all uint64 because they're cumulative, and could quickly wrap
	// around otherwise.
	// mcache.refill mcache.releaseAll 两个方法中增加此计数。
	// 不会减少。
	// tinySpanClass = 5 级别的msapn
	// 这个只是更具体的，其在smallAllocCount中也有对应的项。
	tinyAllocCount  uint64                  // number of tiny allocations
	// 执行 mcache.allocLarge 时分配的对象的大小。
	// 不会减少。
	largeAlloc      uint64                  // bytes allocated for large objects
	// 执行 mcache.allocLarge 时递增此计数(递增1)。
	// 不会减少。
	largeAllocCount uint64                  // number of large object allocations
	// mcache.refill mcache.releaseAll 这两个方法中增加计数。
	// 不会减少。
	smallAllocCount [_NumSizeClasses]uint64 // number of allocs for small objects
	// 清扫完一个mspan之后，发现其是大对象mspan且对象没有被标记，会将此值加上该大对象的字节大小。
	// 不会减少。
	largeFree       uint64                  // bytes freed for large objects (>maxSmallSize)
	// 清扫完一个mspan之后，发现其是大对象mspan且对象没有被标记，会将此值加一。
	// 对象计算(也可以认为是mspan计算)，因为当一个mspan的elemsize>32KB时，其只有
	// 一个对象。
	// 不会减少。
	largeFreeCount  uint64                  // number of frees for large objects (>maxSmallSize)
	// sweepLocked.sweep 函数中增加计数。
	// 在执行sweep时，未标记已经分配的对象个数。
	// 不会减少。
	smallFreeCount  [_NumSizeClasses]uint64 // number of frees for small objects (<=maxSmallSize)

	// NOTE: This struct must be a multiple of 8 bytes in size because it
	// is stored in an array. If it's not, atomic accesses to the above
	// fields may be unaligned and fail on 32-bit platforms.
}

// merge adds in the deltas from b into a.
func (a *heapStatsDelta) merge(b *heapStatsDelta) {
	a.committed += b.committed
	a.released += b.released
	a.inHeap += b.inHeap
	a.inStacks += b.inStacks
	a.inWorkBufs += b.inWorkBufs
	a.inPtrScalarBits += b.inPtrScalarBits

	a.tinyAllocCount += b.tinyAllocCount
	a.largeAlloc += b.largeAlloc
	a.largeAllocCount += b.largeAllocCount
	for i := range b.smallAllocCount {
		a.smallAllocCount[i] += b.smallAllocCount[i]
	}
	a.largeFree += b.largeFree
	a.largeFreeCount += b.largeFreeCount
	for i := range b.smallFreeCount {
		a.smallFreeCount[i] += b.smallFreeCount[i]
	}
}

// consistentHeapStats represents a set of various memory statistics
// whose updates must be viewed completely to get a consistent
// state of the world.
//
// To write updates to memory stats use the acquire and release
// methods. To obtain a consistent global snapshot of these statistics,
// use read.
type consistentHeapStats struct {
	// stats is a ring buffer of heapStatsDelta values.
	// Writers always atomically update the delta at index gen.
	//
	// Readers operate by rotating gen (0 -> 1 -> 2 -> 0 -> ...)
	// and synchronizing with writers by observing each P's
	// statsSeq field. If the reader observes a P not writing,
	// it can be sure that it will pick up the new gen value the
	// next time it writes.
	//
	// The reader then takes responsibility by clearing space
	// in the ring buffer for the next reader to rotate gen to
	// that space (i.e. it merges in values from index (gen-2) mod 3
	// to index (gen-1) mod 3, then clears the former).
	//
	// Note that this means only one reader can be reading at a time.
	// There is no way for readers to synchronize.
	//
	// This process is why we need a ring buffer of size 3 instead
	// of 2: one is for the writers, one contains the most recent
	// data, and the last one is clear so writers can begin writing
	// to it the moment gen is updated.
	stats [3]heapStatsDelta

	// gen represents the current index into which writers
	// are writing, and can take on the value of 0, 1, or 2.
	gen atomic.Uint32

	// noPLock is intended to provide mutual exclusion for updating
	// stats when no P is available. It does not block other writers
	// with a P, only other writers without a P and the reader. Because
	// stats are usually updated when a P is available, contention on
	// this lock should be minimal.
	noPLock mutex
}

// acquire returns a heapStatsDelta to be updated. In effect,
// it acquires the shard for writing. release must be called
// as soon as the relevant deltas are updated.
//
// The returned heapStatsDelta must be updated atomically.
//
// The caller's P must not change between acquire and
// release. This also means that the caller should not
// acquire a P or release its P in between. A P also must
// not acquire a given consistentHeapStats if it hasn't
// yet released it.
//
// nosplit because a stack growth in this function could
// lead to a stack allocation that could reenter the
// function.
//
//go:nosplit
func (m *consistentHeapStats) acquire() *heapStatsDelta {
	if pp := getg().m.p.ptr(); pp != nil {
		seq := pp.statsSeq.Add(1)
		if seq%2 == 0 {
			// Should have been incremented to odd.
			print("runtime: seq=", seq, "\n")
			throw("bad sequence number")
		}
	} else {
		lock(&m.noPLock)
	}
	gen := m.gen.Load() % 3
	return &m.stats[gen]
}

// release indicates that the writer is done modifying
// the delta. The value returned by the corresponding
// acquire must no longer be accessed or modified after
// release is called.
//
// The caller's P must not change between acquire and
// release. This also means that the caller should not
// acquire a P or release its P in between.
//
// nosplit because a stack growth in this function could
// lead to a stack allocation that causes another acquire
// before this operation has completed.
//
//go:nosplit
func (m *consistentHeapStats) release() {
	if pp := getg().m.p.ptr(); pp != nil {
		seq := pp.statsSeq.Add(1)
		if seq%2 != 0 {
			// Should have been incremented to even.
			print("runtime: seq=", seq, "\n")
			throw("bad sequence number")
		}
	} else {
		unlock(&m.noPLock)
	}
}

// unsafeRead aggregates the delta for this shard into out.
//
// Unsafe because it does so without any synchronization. The
// world must be stopped.
func (m *consistentHeapStats) unsafeRead(out *heapStatsDelta) {
	assertWorldStopped()

	for i := range m.stats {
		out.merge(&m.stats[i])
	}
}

// unsafeClear clears the shard.
//
// Unsafe because the world must be stopped and values should
// be donated elsewhere before clearing.
func (m *consistentHeapStats) unsafeClear() {
	assertWorldStopped()

	for i := range m.stats {
		m.stats[i] = heapStatsDelta{}
	}
}

// read takes a globally consistent snapshot of m
// and puts the aggregated value in out. Even though out is a
// heapStatsDelta, the resulting values should be complete and
// valid statistic values.
//
// Not safe to call concurrently. The world must be stopped
// or metricsSema must be held.
//
// 返回值可以一直保留到再次调用read，在此期间此值都不会发生变更。
// 相当于上一次调用read方法时m的一个快照。
func (m *consistentHeapStats) read(out *heapStatsDelta) {
	// Getting preempted after this point is not safe because
	// we read allp. We need to make sure a STW can't happen
	// so it doesn't change out from under us.
	mp := acquirem()

	// Get the current generation. We can be confident that this
	// will not change since read is serialized and is the only
	// one that modifies currGen.
	currGen := m.gen.Load()
	prevGen := currGen - 1
	// 此次read的返回值是stats[currGen]与stats[prevGen]的合并结果。
	if currGen == 0 {
		prevGen = 2
	}

	// Prevent writers without a P from writing while we update gen.
	//
	// 必须上锁因为我们观察不到非p对与m的写操作的状态。
	// p可以通过其statsSeq字段判断。
	lock(&m.noPLock)

	// Rotate gen, effectively taking a snapshot of the state of
	// these statistics at the point of the exchange by moving
	// writers to the next set of deltas.
	//
	// This exchange is safe to do because we won't race
	// with anyone else trying to update this value.
	//
	// 下面的swap是将上一次read方法调用时的prevGen(空的)替换掉m.gen。
	m.gen.Swap((currGen + 1) % 3)
	// 在此之后所有的p都写入到新的m.stats[m.gen]中。
	// 这是快照分界线。

	// Allow P-less writers to continue. They'll be writing to the
	// next generation now.
	unlock(&m.noPLock)

	// 下面的循环等待上面执行swap之前可能存在执行写的所有p结束写。
	// 当然也有可能存在swap调用之后执行写的p不过没关系。
	for _, p := range allp {
		// Spin until there are no more writers.
		for p.statsSeq.Load()%2 != 0 {
		}
	}
	// 此时 m.stats[prev]和m.stats[cur]不会在发生变更。
	// 可以放心的进行合并。

	// At this point we've observed that each sequence
	// number is even, so any future writers will observe
	// the new gen value. That means it's safe to read from
	// the other deltas in the stats buffer.

	// Perform our responsibilities and free up
	// stats[prevGen] for the next time we want to take
	// a snapshot.
	//
	// 合并生成快照。
	m.stats[currGen].merge(&m.stats[prevGen])
	// 情况prevGen，其就是下一次read时用于替换m.gen的值。
	m.stats[prevGen] = heapStatsDelta{}

	// Finally, copy out the complete delta.
	//
	// 返回赋值。
	*out = m.stats[currGen]

	releasem(mp)
}

type cpuStats struct {
	// All fields are CPU time in nanoseconds computed by comparing
	// calls of nanotime. This means they're all overestimates, because
	// they don't accurately compute on-CPU time (so some of the time
	// could be spent scheduled away by the OS).

	gcAssistTime    int64 // GC assists
	gcDedicatedTime int64 // GC dedicated mark workers + pauses
	gcIdleTime      int64 // GC idle mark workers
	gcPauseTime     int64 // GC pauses (all GOMAXPROCS, even if just 1 is running)
	// idle/assist/didecated/fractional GC 执行时间 + 清扫终止阶段(STW持续时间)+标记终止阶段(STW持续时间)
	gcTotalTime     int64

	scavengeAssistTime int64 // background scavenger
	scavengeBgTime     int64 // scavenge assists
	scavengeTotalTime  int64

	idleTime int64 // Time Ps spent in _Pidle.
	userTime int64 // Time Ps spent in _Prunning or _Psyscall that's not any of the above.

	// 程序启动之后不同的 GOMAXPROCS * 此GOMAXPROCS持续的时间的总和。同样的GOMAXPROCS可以出现多次只要调用了 procresize 函数，
	// 包含现在的 GOMAXPROCS * 此GOMAXPROCS。
	totalTime int64 // GOMAXPROCS * (monotonic wall clock time elapsed)
}
