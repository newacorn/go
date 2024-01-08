// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Page heap.
//
// See malloc.go for overview.

package runtime

import (
	"internal/cpu"
	"internal/goarch"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// minPhysPageSize is a lower-bound on the physical page size. The
	// true physical page size may be larger than this. In contrast,
	// sys.PhysPageSize is an upper-bound on the physical page size.
	minPhysPageSize = 4096

	// maxPhysPageSize is the maximum page size the runtime supports.
	maxPhysPageSize = 512 << 10

	// maxPhysHugePageSize sets an upper-bound on the maximum huge page size
	// that the runtime supports.
	maxPhysHugePageSize = pallocChunkBytes

	// pagesPerReclaimerChunk indicates how many pages to scan from the
	// pageInUse bitmap at a time. Used by the page reclaimer.
	//
	// Higher values reduce contention on scanning indexes (such as
	// h.reclaimIndex), but increase the minimum latency of the
	// operation.
	//
	// The time required to scan this many pages can vary a lot depending
	// on how many spans are actually freed. Experimentally, it can
	// scan for pages at ~300 GB/ms on a 2.6GHz Core i7, but can only
	// free spans at ~32 MB/ms. Using 512 pages bounds this at
	// roughly 100µs.
	//
	// Must be a multiple of the pageInUse bitmap element size and
	// must also evenly divide pagesPerArena.
	pagesPerReclaimerChunk = 512

	// physPageAlignedStacks indicates whether stack allocations must be
	// physical page aligned. This is a requirement for MAP_STACK on
	// OpenBSD.
	physPageAlignedStacks = GOOS == "openbsd"
)

// Main malloc heap.
// The heap itself is the "free" and "scav" treaps,
// but all the other global data is here too.
//
// mheap must not be heap-allocated because it contains mSpanLists,
// which must not be heap-allocated.
type mheap struct {
	_ sys.NotInHeap

	// lock must only be acquired on the system stack, otherwise a g
	// could self-deadlock if its stack grows with the lock held.
	lock mutex

	pages pageAlloc // page allocation data structure

	sweepgen uint32 // sweep generation, see comment in mspan; written during STW

	// allspans is a slice of all mspans ever created. Each mspan
	// appears exactly once.
	//
	// The memory for allspans is manually managed and can be
	// reallocated and move as the heap grows.
	//
	// In general, allspans is protected by mheap_.lock, which
	// prevents concurrent access as well as freeing the backing
	// store. Accesses during STW might not hold the lock, but
	// must ensure that allocation cannot happen around the
	// access (since that may free the backing store).
	allspans []*mspan // all spans out there

	// Proportional sweep
	//
	// These parameters represent a linear function from gcController.heapLive
	// to page sweep count.
	// The proportional sweep system works to
	// stay in the black by keeping the current page sweep count
	// above this line at the current gcController.heapLive.
	//
	// The line has slope sweepPagesPerByte and passes through a
	// basis point at (sweepHeapLiveBasis, pagesSweptBasis). At
	// any given time, the system is at (gcController.heapLive,
	// pagesSwept) in this space.
	//
	// It is important that the line pass through a point we
	// control rather than simply starting at a 0,0 origin
	// because that lets us adjust sweep pacing at any time while
	// accounting for current progress. If we could only adjust
	// the slope, it would create a discontinuity in debt if any
	// progress has already been made.
	pagesInUse         atomic.Uintptr // pages of spans in stats mSpanInUse
	// 只要对msapn执行了sweep操作，无论清理结果如何。都会将 mspan.npages 增加到此值。
	pagesSwept         atomic.Uint64  // pages swept this cycle
	pagesSweptBasis    atomic.Uint64  // pagesSwept to use as the origin of the sweep ratio
	sweepHeapLiveBasis uint64         // value of gcController.heapLive to use as the origin of sweep ratio; written with lock, read without
	sweepPagesPerByte  float64        // proportional sweep ratio; written with lock, read without

	// Page reclaimer state

	// reclaimIndex is the page index in allArenas of next page to
	// reclaim. Specifically, it refers to page (i %
	// pagesPerArena) of arena allArenas[i / pagesPerArena].
	//
	// If this is >= 1<<63, the page reclaimer
	// is done scanning the page marks.(页面回收器已完成对页面标记的扫描。)
	reclaimIndex atomic.Uint64

	// reclaimCredit is spare credit for extra pages swept. Since
	// the page reclaimer works in large chunks, it may reclaim
	// more than requested. Any spare pages released go to this
	// credit pool.
	// 页面回收器回收的页面超过请求部分(页数)存储在这里。
	reclaimCredit atomic.Uintptr

	// arenas is the heap arena map. It points to the metadata for
	// the heap for every arena frame of the entire usable virtual
	// address space.
	//
	// Use arenaIndex to compute indexes into this array.
	//
	// For regions of the address space that are not backed by the
	// Go heap, the arena map contains nil.
	//
	// Modifications are protected by mheap_.lock. Reads can be
	// performed without locking; however, a given entry can
	// transition from nil to non-nil at any time when the lock
	// isn't held. (Entries never transitions back to nil.)
	//
	// In general, this is a two-level mapping consisting of an L1
	// map and possibly many L2 maps. This saves space when there
	// are a huge number of arena frames. However, on many
	// platforms (even 64-bit), arenaL1Bits is 0, making this
	// effectively a single-level map. In this case, arenas[0]
	// will never be nil.
	//
	// arena 索引到其元数据 ( heapArena 类型）的映射。
	// Linux 平台 第一维长度为0，第二维维大小为：48-26-0 = 22; 26为arena大小64MB的2的幂。2^26=64MB
	arenas [1 << arenaL1Bits]*[1 << arenaL2Bits]*heapArena

	// heapArenaAlloc is pre-reserved space for allocating heapArena
	// objects. This is only used on 32-bit, where we pre-reserve
	// this space to avoid interleaving it with the heap itself.
	//
	// 仅在32位平台上使用，其保留空间用于分配 heapArena 数据结构。
	heapArenaAlloc linearAlloc

	// arenaHints is a list of addresses at which to attempt to
	// add more heap arenas. This is initially populated with a
	// set of general hint addresses, and grown with the bounds of
	// actual heap arena ranges.
	//
	// arena 内存分配的起始地址列表。
	// p=0x[00-0x7F]c000000000
	// mallocinit
	// 在 mallocinit 函数中初始化，共初始化128个arenaHint实例，它们使用arenaHint.next串联。
	// 起始的 arenaHint.addr 是0x00c000000000，其next的addr是0x00c100000000一直到0x7f0000000000。
	//
	// 没有开启race检测的话：
	// 0x[40-7F]c000000000 for userArena
	// 0x[0-3F]c000000000 for normalArean
	arenaHints *arenaHint

	// arena is a pre-reserved space for allocating heap arenas
	// (the actual arenas). This is only used on 32-bit.
	//
	// 32位平台使用，预保留空间用于分配 heap arena。
	arena linearAlloc

	// allArenas is the arenaIndex of every mapped arena. This can
	// be used to iterate through the address space.
	//
	// Access is protected by mheap_.lock. However, since this is
	// append-only and old backing arrays are never freed, it is
	// safe to acquire mheap_.lock, copy the slice header, and
	// then release mheap_.lock.
	//
	// 已经映射的 arena 索引切片。
	// amd64: 初始大小为 cap(allArenas)=4096(phyicpagesize/8)=512
	// 在 sysAlloc 调用中初始化。
	allArenas []arenaIdx

	// sweepArenas is a snapshot of allArenas taken at the
	// beginning of the sweep cycle.
	//
	// This can be read safely by
	// simply blocking GC (by disabling preemption).
	//
	// sweep 开始之前， allArenas 的快照。
	sweepArenas []arenaIdx

	// markArenas is a snapshot of allArenas taken at the beginning
	// of the mark cycle. Because allArenas is append-only, neither
	// this slice nor its contents will change during the mark, so
	// it can be read safely.
	//
	// 标记开始之前对 allArenas 的快照。
	markArenas []arenaIdx

	// curArena is the arena that the heap is currently growing
	// into. This should always be physPageSize-aligned.
	//
	// 堆正在其上增长的 arena。
	curArena struct {
		base, end uintptr
	}

	// central free lists for small size classes.
	// the padding makes sure that the mcentrals are
	// spaced CacheLinePadSize bytes apart, so that each mcentral.lock
	// gets its own cache line.
	// central is indexed by spanClass.
	central [numSpanClasses]struct {
		mcentral mcentral
		// 使整个 struct 大小为 cpu.CacheLinePadSize (64) 的整数倍。
		pad      [(cpu.CacheLinePadSize - unsafe.Sizeof(mcentral{})%cpu.CacheLinePadSize) % cpu.CacheLinePadSize]byte
	}

	// mspan 结构体分配器
	spanalloc             fixalloc // allocator for span*
	// mcache 结构体分配器
	cachealloc            fixalloc // allocator for mcache*
	specialfinalizeralloc fixalloc // allocator for specialfinalizer*
	specialprofilealloc   fixalloc // allocator for specialprofile*
	specialReachableAlloc fixalloc // allocator for specialReachable
	//
	speciallock           mutex    // lock for special record allocators.
	arenaHintAlloc        fixalloc // allocator for arenaHints

	// User arena state.
	//
	// 见 arena package，属于实验性质构建标签。
	// 一个arena里只有一个msapn，用于申请一块内存创建一些变量，然后统一释放。
	// func main() {
	//	a := arena.NewArena()
	//	c := arena.New[int](a)
	//	fmt.Printf("%T", c)
	// }
	// Protected by mheap_.lock.
	userArena struct {
		// arenaHints is a list of addresses at which to attempt to
		// add more heap arenas for user arena chunks. This is initially
		// populated with a set of general hint addresses, and grown with
		// the bounds of actual heap arena ranges.
		arenaHints *arenaHint

		// quarantineList is a list of user arena spans that have been set to fault, but
		// are waiting for all pointers into them to go away.
		// Sweeping handles
		// identifying when this is true, and moves the span to the ready list.
		//
		// 等待所有指向列表里 mspan 的指针移除， Sweeping 会将这个 mspan 移到 readyList 中。
		quarantineList mSpanList

		// readyList is a list of empty user arena spans that are ready for reuse.
		readyList mSpanList
	}

	unused *specialfinalizer // never set, just here to force the specialfinalizer type into DWARF
}

var mheap_ mheap
// **heapArena 是在GO的堆之外分配和管理的**
//
// Go 语言的 runtime 将堆空间划分成多个 arena, 在 amd64 架构的 Linux 环境下，每个arena的大小是64MB，起始地址
// 也是对齐到64MB的。每个arena都有一个与之对应的 heapArena 结构，用来存储 arena 的元数据。
//
// A heapArena stores metadata for a heap arena. heapArenas are stored
// outside of the Go heap and accessed via the mheap_.arenas index.
type heapArena struct {
	_ sys.NotInHeap

	// bitmap stores the pointer/scalar bitmap for the words in
	// this arena. See mbitmap.go for a description.
	// This array uses 1 bit per word of heap, or 1.6% of the heap size (for 64-bit).
	//
	// heapArenaBitmapWords = 64MB/8/8/8(unix)=128KB；
	// 每个bit对应一个指针大小的内存，表示指针/标量位，即这个内存上存储的是否是指针。
	// 第一个元素的第0位位对应此arena从起始地址开始的第一个word。
	// bitmap 的每个元素可以对应8个word。
	// 每个 entry 代表 64*8 = 512byte内存(arena中的)
	bitmap [heapArenaBitmapWords]uintptr

	// If the ith bit of noMorePtrs is true, then there are no more
	// pointers for the object containing the word described by the
	// high bit of bitmap[i].
	// In that case, bitmap[i+1], ... must be zero until the start
	// of the next object.
	//
	// We never operate on these entries using bit-parallel techniques,
	// so it is ok if they are small.
	// Also, they can't be bigger than
	// uint16 because at that size a single noMorePtrs entry
	// represents 8K of memory, the minimum size of a span. Any larger
	// and we'd have to worry about concurrent updates.
	//
	// This array uses 1 bit per word of bitmap, or .024% of the heap size (for 64-bit).
	//
	// 每个entry 表示 512kb*8=4096byte 内存(arena中的)
	// noMorePtrs 元素中的每个位对应 bitmap 中的一个元素。
	//
	noMorePtrs [heapArenaBitmapWords / 8]uint8

	// spans maps from virtual address page ID within this arena to *mspan.
	//
	// For allocated spans, their pages map to the span itself.
	// For free spans, only the lowest and highest pages map to the span itself.
	//
	// Internal pages map to an arbitrary span.
	// For pages that have never been allocated, spans entries are nil.
	//
	// Modifications are protected by mheap.lock. Reads can be
	// performed without locking, but ONLY from indexes that are
	// known to contain in-use or stack spans.
	//
	// This means there
	// must not be a safe-point between establishing that an
	// address is live and looking it up in the spans array.
	//
	// pagesPerArena = 64MB/8192=8192
	// 页到 *mspan 的映射，因为一个页可以*msapn可以跨多个页，所以会有索引不同
	// 元素相同的情况。
	spans [pagesPerArena]*mspan

	// pageInUse is a bitmap that indicates which spans are in
	// state mSpanInUse. This bitmap is indexed by page number,
	// but only the bit corresponding to the first page in each
	// span is used.
	//
	// Reads and writes are atomic.
	//
	// pageInUse 是长度为1024的 uint8 数组，实际上被用作一个8192位的位图，通过它和 spans 可以
	// 快速地找到那些处于 mSpanInUse 状态的 mspan 。虽然 pageInUse 位图为 arena 中的每个页面
	// 都提供了一个二进制位，但是对于那些包含多个页面的 mspan，只有第一个页面对应的二进制位会被用
	// 到，标记整个 span。
	// 数组大小为：8192/8=1024。
	// 每个元素的每个bit位对应一个页。
	pageInUse [pagesPerArena / 8]uint8

	// pageMarks is a bitmap that indicates which spans have any
	// marked objects on them. Like pageInUse, only the bit
	// corresponding to the first page in each span is used.
	//
	// Writes are done atomically during marking. Reads are
	// non-atomic and lock-free since they only occur during
	// sweeping (and hence never race with writes).
	//
	// This is used to quickly find whole spans that can be freed.
	//
	// TODO(austin): It would be nice if this was uint64 for
	// faster scanning, but we don't have 64-bit atomic bit
	// operations.
	//
	// pageMarks 表示哪些 span 中存在被标记的对象，与 pageInUse 一样用与
	// 起始页面对应的二进制位来标记整个 mspan。 在GC标记阶段会原子性地修改这
	// 个位图，标记结束之后就不会再进行改动了。
	// 清扫阶段如果发现某个 mspan 不存在被标记的对象，就可以释放整个 mspan 了。
	// **和 g.gcAssistBytes(详见) 在同一个地方进行重置(每轮GC开始时，上一轮清扫结束STW之前)**
	// 1024大小的数组
	//
	// 每轮GC开始时此字段在gcstart函数调用中被清空。
	pageMarks [pagesPerArena / 8]uint8

	// pageSpecials is a bitmap that indicates which spans have
	// specials (finalizers or other). Like pageInUse, only the bit
	// corresponding to the first page in each span is used.
	//
	// Writes are done atomically whenever a special is added to
	// a span and whenever the last special is removed from a span.
	// Reads are done atomically to find spans containing specials
	// during marking.
	//
	// pageSpecials 又是一个与 pageInUse 类似的位图，只不过标记的是哪些 mspan 中包含
	// 特殊设置，目前主要指的是包含 finalizers ，或者 runtime 内部用来存储 heap profile
	// 数据的 bucket。
	//
	pageSpecials [pagesPerArena / 8]uint8

	// checkmarks stores the debug.gccheckmark state. It is only
	// used if debug.gccheckmark > 0.
	// checkmarks 是一个大小为 [1MB]uint8 的位图，其中每个二进制位对应 arena 中一个指针大小的
	// 内存单元。当开启调试 debug.gcheckmark 的时候， checkmarks 位图用来存储 GC标记的数据，该
	// 调试模式会在 STW 的状态下遍历对象图，用来校验并发回收器能够正确地标记所有存活对象。
	//
	// 用线性模式校验并发模式的正确性，可以多标记不能少标记。
	checkmarks *checkmarksMap

	// zeroedBase marks the first byte of the first page in this
	// arena which hasn't been used yet and is therefore already
	// zero. zeroedBase is relative to the arena base.
	// Increases monotonically until it hits heapArenaBytes.
	//
	// This field is sufficient to determine if an allocation
	// needs to be zeroed because the page allocator follows an
	// address-ordered first-fit policy.
	//
	// Read atomically and written with an atomic CAS.
	//
	// zeroedBase 记录的是当前 arena 中下个还未被使用的页的位置，相对于arena
	// 的起始地址的偏移量。
	// 页面分配器会安装地址顺序分配页面，所以 zeroedBase 之
	// 后的页面都还没有被用到，因此还都保持着清零的状态。通过它可以快速判断分配的
	// 内存是否还需要进行清零。
	//
	// 绝对地址为: 此arnea在mheap中的索引*64MB+zeroedBase。
	zeroedBase uintptr
}

// Go的堆是动态增长的，初始化的时候并不会向操作系统预先申请一些内存备用，而是
// 等到实际用到的时候才去分配。
// 为避免随机申请内存造成进程的虚拟地址空间混乱不堪，我们要让堆从一个起始地址
// 连续地增长，而 arenaHint 结构就是用来做这件事的，它提示分配器从哪里开始
// 分配内存来扩展堆，尽量使用堆按照预期的方式增长。
//
// sysAlloc() 函数根据当前 arenaHint 的指示来扩展空堆空间，当申请遇到错误时
// 会自动切换至下一个 arenaHint 。
//
// arenaHint is a hint for where to grow the heap arenas. See
// mheap_.arenaHints.
type arenaHint struct {
	_    sys.NotInHeap
	// 可用空间的起始地址
	addr uintptr
	// 当 down 为true时， addr 表示可用区间的最高地址，类似数学上的右开区间。
	// 当 down 为false时， addr 表示可用区间的低地址，类似数学上的左闭区间。
	down bool
	// next 表示链表中的下一个 arenaHint。
	next *arenaHint
}

// An mspan is a run of pages.
//
// When a mspan is in the heap free treap, state == mSpanFree
// and heapmap(s->start) == span, heapmap(s->start+s->npages-1) == span.
// If the mspan is in the heap scav treap, then in addition to the
// above scavenged == true. scavenged == false in all other cases.
//
// When a mspan is allocated, state == mSpanInUse or mSpanManual
// and heapmap(i) == span for all s->start <= i < s->start+s->npages.

// Every mspan is in one doubly-linked list, either in the mheap's
// busy list or one of the mcentral's span lists.

// An mspan representing actual memory has state mSpanInUse,
// mSpanManual, or mSpanFree. Transitions between these states are
// constrained as follows:
//
//   - A span may transition from free to in-use or manual during any GC
//     phase.
//
//   - During sweeping (gcphase == _GCoff), a span may transition from
//     in-use to free (as a result of sweeping) or manual to free (as a
//     result of stacks being freed).
//
//   - During GC (gcphase != _GCoff), a span *must not* transition from
//     manual or in-use to free. Because concurrent GC may read a pointer
//     and then look up its span, the span state must be monotonic.
//
// Setting mspan.state to mSpanInUse or mSpanManual must be done
// atomically and only after all other span fields are valid.
// Likewise, if inspecting a span is contingent on it being
// mSpanInUse, the state should be loaded atomically and checked
// before depending on other fields. This allows the garbage collector
// to safely deal with potentially invalid pointers, since resolving
// such pointers may race with a span being allocated.
type mSpanState uint8

const (
	mSpanDead   mSpanState = iota
	mSpanInUse             // allocated for garbage collected heap
	mSpanManual            // allocated for manual management (e.g., stack allocator)
)

// mSpanStateNames are the names of the span states, indexed by
// mSpanState.
var mSpanStateNames = []string{
	"mSpanDead",
	"mSpanInUse",
	"mSpanManual",
}

// mSpanStateBox holds an atomic.Uint8 to provide atomic operations on
// an mSpanState. This is a separate type to disallow accidental comparison
// or assignment with mSpanState.
type mSpanStateBox struct {
	s atomic.Uint8
}

// It is nosplit to match get, below.

//go:nosplit
func (b *mSpanStateBox) set(s mSpanState) {
	b.s.Store(uint8(s))
}

// It is nosplit because it's called indirectly by typedmemclr,
// which must not be preempted.

//go:nosplit
func (b *mSpanStateBox) get() mSpanState {
	return mSpanState(b.s.Load())
}

// mSpanList heads a linked list of spans.
// 链接 mspan 的双向链表的表头。
//
type mSpanList struct {
	_     sys.NotInHeap
	first *mspan // first span in list, or nil if none
	last  *mspan // last span in list, or nil if none
}

type mspan struct {
	_    sys.NotInHeap
	// next 和 prev 用来构建mspan双链表
	next *mspan     // next span in list, or nil if none
	prev *mspan     // previous span in list, or nil if none
	// list 指向双链表的链表头
	list *mSpanList // For debugging. TODO: Remove.

	// 指向当前 mspan 的第一个对象/第一个页的起始地址。
	startAddr uintptr // address of first byte of span aka s.base()
	// 记录的是当前 mspan 中有几个页面，乘以页面的大小就可以得出 mspan 空间的大小。
	npages    uintptr // number of pages in span

	// 是个单链表，在 mSpanManual 类型的 span 中，用来串联所有空闲的对象。
	// 类型 gclinkptr 底层是个 uintptr ，它把每个空闲对象头部的一个 uintptr 用作指向
	// 下一个对象的指针。
	manualFreeList gclinkptr // list of free objects in mSpanManual spans

	// freeindex is the slot index between 0 and nelems at which to begin scanning
	// for the next free object in this span.
	// Each allocation scans allocBits starting at freeindex until it encounters a 0
	// indicating a free object. freeindex is then adjusted so that subsequent scans begin
	// just past the newly discovered free object.
	//
	// If freeindex == nelem, this span has no free objects.
	//
	// allocBits is a bitmap of objects in this span.
	// If n >= freeindex and allocBits[n/8] & (1<<(n%8)) is 0
	// then object n is free;
	// otherwise, object n is allocated. Bits starting at nelem are
	// undefined and should never be referenced.
	//
	// Object n starts at address n*elemsize + (start << pageShift).
	//
	// 是预期的下个空闲对象的索引，取值范围在0和 nelems 之间，下次分配时会从这个索引
	// 开始向后扫描，假如发现第 N 个对象是空闲的，就将其用于分配，并会把 freeindex 更新
	// 为 N+1。
	//
	// freeindex 每递增64便会对应一个新的 allocBits 数组元素
	freeindex uintptr
	// TODO: Look up nelems from sizeclass and remove this field if it
	// helps performance.
	//
	// 记录的是当前 span 被划分成了多少个内存块。
	nelems uintptr // number of object in the span.

	// Cache of the allocBits at freeindex. allocCache is shifted
	// such that the lowest bit corresponds to the bit freeindex.
	// allocCache holds the complement of allocBits, thus allowing
	// ctz (count trailing zero) to use it directly.
	// allocCache may contain bits beyond s.nelems; the caller must ignore
	// these.
	//
	// allocCache 缓存了 allocBits 中从 freeindex 开始的64个二进制位，
	// 这样一来在实际分配时更高效。
	// 假设 allocCache 中n个位为1，就表示在重新填充allocCache之前，在区间[freeindex,freeindex+n)范围内
	// 的索引都对应一个有效未使用过的obj，但索引最大值能超过 nelems 的值。
	allocCache uint64

	// allocBits and gcmarkBits hold pointers to a span's mark and
	// allocation bits. The pointers are 8 byte aligned.
	// There are three arenas where this data is held.
	// free: Dirty arenas that are no longer accessed
	//       and can be reused.
	// next: Holds information to be used in the next GC cycle.
	// current: Information being used during this GC cycle.
	// previous: Information being used during the last GC cycle.
	// A new GC cycle starts with the call to finishsweep_m.
	// finishsweep_m moves the previous arena to the free arena,
	// the current arena to the previous arena, and
	// the next arena to the current arena.
	// The next arena is populated as the spans request
	// memory to hold gcmarkBits for the next GC cycle as well
	// as allocBits for newly allocated spans.
	//
	// The pointer arithmetic is done "by hand" instead of using
	// arrays to avoid bounds checks along critical performance
	// paths.
	// The sweep will free the old allocBits and set allocBits to the
	// gcmarkBits. The gcmarkBits are replaced with a fresh zeroed
	// out memory.
	//
	// 指向当前 span 的分配位图
	// 每个二进制位对应 span 中的一个内存块
	// 给定当前 span 中一个内存块索引n，如果 n>= freeindex，并且
	// bp,mask:=gcBits.bitp(n)
	// *bp & mask ==0 ，则表示内存块就是空闲。
	//
	// 清扫阶段会释放旧的 allocBits ，然后把 gcmarkBits 作用 allocBits,并为 gcmarkBits
	// 重新分配一段清零的内存。
	allocBits  *gcBits
	// 指向当前 span 的标记位图
	gcmarkBits *gcBits

	// sweep generation:
	//
	// ** 下一轮sweep，h->sweepgen 更新为 h->sweepgen+2，这样 msapn.sweepgen就小二了。
	// if sweepgen == h->sweepgen - 2, the span needs sweeping
	//
	// 写入
	// tryAcquire 方法中获取 mspan 的清理权，将 sweepgen 由 h->sweepgen -2 替换为
	// h->sweepgen -1。
	// mcentral.uncacheSpan 中如果检测到 seepgen = h->sweegen+1，会将 sweepgen 替换为 h->sweepgen-1
	// if sweepgen == h->sweepgen - 1, the span is currently being swept
	//
	// 写入
	// 扫描完成时，将 sweepgen 由h->sweepgen-1 更为 h->sweepgen
	// if sweepgen == h->sweepgen, the span is swept and ready to use
	//
	// ** mspan cached之后下一轮执行sweep， h->sweepgen=h->sweepgen+2 比 mspan.sweepgen 小1。
	// if sweepgen == h->sweepgen + 1, the span was cached before sweep began and is still cached, and needs sweeping
	//
	// 写入
	// 在mcache.refill 方法中，新申请的mspan会设置 s.sweepgen = mheap_.swwepgen+3
	// if sweepgen == h->sweepgen + 3, the span was swept and then cached and is still cached
	//
	// h->sweepgen is incremented by 2 after every GC

	// sweepgen 与 mheap.sweepgen 相比较，能够得知当前 span 处于清扫、清扫中、已清扫
	// 等哪种状态。
	sweepgen              uint32

	// 用来优化整数除法运算的
	divMul                uint32        // for divide by elemsize
	// 用于记录当前span 中有多少内存块被分配了。
	// 如果 allocCount = nelems 则 mspan 没有空闲obj了。
	allocCount            uint16        // number of allocated objects

	// 类似于 sizeclass，实际上它把 sizeclass 左移了一位，用最低位记录是否不需要扫描
	// ，称为 noscan。
	//
	// Go 为同一种 sizeclass 提供了两种span，一种用来分配包含指针对象，另一种用来分配不包含
	// 指针的对象。
	// 这样一来不包含指针的span就不用进行进一步扫描了，noscan位就是这个意思。
	spanclass             spanClass     // size class and noscan (uint8)

	// 记录的是当前 span 的状态，有 mSpanDead、 mSpanInUse 和 mSpanManual 这三种取值
	// ,分别表无效的 mspan、被GC自动管理的span 和手动管理的span。
	//
	// goroutine 的栈分配的就是 mSpanManual状态的span。
	state                 mSpanStateBox // mSpanInUse etc; accessed atomically (get/set methods)
	// 表明分配之前需要对内存进行清零。
	// 在清扫一个msapn时，如果存在被清理的对象(分配未标记)。则此值会设置为1。
	needzero              uint8         // needs to be zeroed before allocation
	isUserArenaChunk      bool          // whether or not this span represents a user arena
	// 记录 mcache 从 mcentral 中申请这个 mspan 时它的 allocCount 字段。
	// 对于新申请的 mspan allocCountBeforeCache = allocCount
	allocCountBeforeCache uint16        // a copy of allocCount that is stored just before this span is cached
	// obj的大小。可以通过 spanclass 计算得到。
	elemsize              uintptr       // computed from sizeclass or from npages
	// 记录的是 span 区间的结束地址，右开区间。
	// startAddr + elemsize * nelems ，并不是其占用的最有一页中的最后一个字节的地址。
	limit                 uintptr       // end of data in span
	// 用来保护这个链表。
	speciallock           mutex         // guards specials list
	// 是个链表，用来记录添加的 finalizer 等。
	specials              *special      // linked list of special records sorted by offset.
	//
	userArenaChunkFree    addrRange     // interval for managing chunk allocation

	// freeIndexForScan is like freeindex, except that freeindex is
	// used by the allocator whereas freeIndexForScan is used by the
	// GC scanner.
	//
	// They are two fields so that the GC sees the object
	// is allocated only when the object and the heap bits are
	// initialized (see also the assignment of freeIndexForScan in
	// mallocgc, and issue 54596).
	freeIndexForScan uintptr
}

func (s *mspan) base() uintptr {
	return s.startAddr
}

func (s *mspan) layout() (size, n, total uintptr) {
	total = s.npages << _PageShift
	size = s.elemsize
	if size > 0 {
		n = total / size
	}
	return
}

// recordspan adds a newly allocated span to h.allspans.
//
// This only happens the first time a span is allocated from
// mheap.spanalloc (it is not called when a span is reused).
//
// Write barriers are disallowed here because it can be called from
// gcWork when allocating new workbufs. However, because it's an
// indirect call from the fixalloc initializer, the compiler can't see
// this.
//
// The heap lock must be held.
//
//go:nowritebarrierrec
func recordspan(vh unsafe.Pointer, p unsafe.Pointer) {
	h := (*mheap)(vh)
	s := (*mspan)(p)

	assertLockHeld(&h.lock)

	if len(h.allspans) >= cap(h.allspans) {
		n := 64 * 1024 / goarch.PtrSize
		if n < cap(h.allspans)*3/2 {
			n = cap(h.allspans) * 3 / 2
		}
		var new []*mspan
		sp := (*slice)(unsafe.Pointer(&new))
		sp.array = sysAlloc(uintptr(n)*goarch.PtrSize, &memstats.other_sys)
		if sp.array == nil {
			throw("runtime: cannot allocate memory")
		}
		sp.len = len(h.allspans)
		sp.cap = n
		if len(h.allspans) > 0 {
			copy(new, h.allspans)
		}
		oldAllspans := h.allspans
		*(*notInHeapSlice)(unsafe.Pointer(&h.allspans)) = *(*notInHeapSlice)(unsafe.Pointer(&new))
		if len(oldAllspans) != 0 {
			sysFree(unsafe.Pointer(&oldAllspans[0]), uintptr(cap(oldAllspans))*unsafe.Sizeof(oldAllspans[0]), &memstats.other_sys)
		}
	}
	h.allspans = h.allspans[:len(h.allspans)+1]
	h.allspans[len(h.allspans)-1] = s
}

// A spanClass represents the size class and noscan-ness of a span.
//
// Each size class has a noscan spanClass and a scan spanClass. The
// noscan spanClass contains only noscan objects, which do not contain
// pointers and thus do not need to be scanned by the garbage
// collector.
//
// 取值范围：[0-135]
//
// 编号为0的spanClass表示其对应的mspan管理的是大对象超过32KB。这样的mspan只包含一个
// 对象。
// sizeClass 有68种，编号为0-67
// 每个编号向左移一位，空位为0表示scan，为1表示noscan。
// 这样 spanClass 就有了136种。
type spanClass uint8

const (
	numSpanClasses = _NumSizeClasses << 1
	tinySpanClass  = spanClass(tinySizeClass<<1 | 1)
)

func makeSpanClass(sizeclass uint8, noscan bool) spanClass {
	return spanClass(sizeclass<<1) | spanClass(bool2int(noscan))
}

func (sc spanClass) sizeclass() int8 {
	return int8(sc >> 1)
}

func (sc spanClass) noscan() bool {
	return sc&1 != 0
}

// arenaIndex()
//
// 在amd64架构的Linux环境下，arena的大小和对齐边界都是64MB，所以整个虚拟地址空间都可以看作由
// 一系列 arena 组成的。 arena的起始地址被定义为常量  arenaBaseOffset。
// 用一个给定地址p减去 arenaBaseOffset，然后除以arena的大小 heapArenaBytes 就可以得到p所在
// 的arena的编号。
// 反之，给定arena的编号，也能由此计算出arena的地址。
//
// arenaIndex returns the index into mheap_.arenas of the arena
// containing metadata for p. This index combines of an index into the
// L1 map and an index into the L2 map and should be used as
// mheap_.arenas[ai.l1()][ai.l2()].
//
// If p is outside the range of valid heap addresses, either l1() or
// l2() will be out of bounds.
//
// It is nosplit because it's called by spanOf and several other
// nosplit functions.
//
//
//go:nosplit
func arenaIndex(p uintptr) arenaIdx {
	return arenaIdx((p - arenaBaseOffset) / heapArenaBytes)
}

// arenaBase returns the low address of the region covered by heap
// arena i.
func arenaBase(i arenaIdx) uintptr {
	return uintptr(i)*heapArenaBytes + arenaBaseOffset
}
// 表示 arena 在堆中所有arena中的序列号，从0开始。
// 这个序列号也用于映射到描述对应arena元数据的 heapArena。
type arenaIdx uint

// l1()
// 返回的是 i 对应的arena的元数据 heapArena 在 mheap.arenas 数组中第一维的索引。
// l1 returns the "l1" portion of an arenaIdx.
//
// Marked nosplit because it's called by spanOf and other nosplit
// functions.
//
//go:nosplit
func (i arenaIdx) l1() uint {
	if arenaL1Bits == 0 {
		// Let the compiler optimize this away if there's no
		// L1 map.
		//
		// linux 平台，第一位索引为0。
		return 0
	} else {
		// 右移20位，Windows平台。
		// 右移之后的结果最多只能低6位上为1。
		return uint(i) >> arenaL1Shift
	}
}

// l2()
// 返回的是 i 对应的arena的元数据 heapArena 在 mheap.arenas 数组中第二维的索引。
// l2 returns the "l2" portion of an arenaIdx.
//
// Marked nosplit because it's called by spanOf and other nosplit funcs.
// functions.
//
//go:nosplit
func (i arenaIdx) l2() uint {
	if arenaL1Bits == 0 {
		// linux 平台，i最多只能有低22位上可以为1。
		return uint(i)
	} else {
		// 取i的低20位
		return uint(i) & (1<<arenaL2Bits - 1)
	}
}

// inheap reports whether b is a pointer into a (potentially dead) heap object.
// It returns false for pointers into mSpanManual spans.
// Non-preemptible because it is used by write barriers.
//
//go:nowritebarrier
//go:nosplit
func inheap(b uintptr) bool {
	return spanOfHeap(b) != nil
}

// inHeapOrStack is a variant of inheap that returns true for pointers
// into any allocated heap span.
//
//go:nowritebarrier
//go:nosplit
func inHeapOrStack(b uintptr) bool {
	s := spanOf(b)
	if s == nil || b < s.base() {
		return false
	}
	switch s.state.get() {
	case mSpanInUse, mSpanManual:
		return b < s.limit
	default:
		return false
	}
}

// spanOf()
// 根据地址p，返回其所在的 mspan
// 先用p求出 arenaIdx,
// 再根据 arenaIdx 从 mheap.arenas 得出 heapArena
// (p/ pageSize )% pagesPerArena 得到 p 所在的页在arena中的偏移量i。
// 根据 heapArena.spans[i] 便可求出对应的 mspan。
// 但首先要验证p表示的地址的合法性。
//
// spanOf returns the span of p. If p does not point into the heap
// arena or no span has ever contained p, spanOf returns nil.
//
// If p does not point to allocated memory, this may return a non-nil
// span that does *not* contain p. If this is a possibility, the
// caller should either call spanOfHeap or check the span bounds
// explicitly.
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOf(p uintptr) *mspan {
	// This function looks big, but we use a lot of constant
	// folding around arenaL1Bits to get it under the inlining
	// budget. Also, many of the checks here are safety checks
	// that Go needs to do anyway, so the generated code is quite
	// short.
	ri := arenaIndex(p)
	if arenaL1Bits == 0 {
		// If there's no L1, then ri.l1() can't be out of bounds but ri.l2() can.
		// Linux 平台下 ri 的有效位超过了22位。
		if ri.l2() >= uint(len(mheap_.arenas[0])) {
			return nil
		}
		// 上面的if已经包含了 arenaL1Bits==0&&l2==nil的情况。
		// 如果l2==nil，则ri.l2()肯定会大于等于uint(len(mheap_.arenas[0]))=0。
		// if arenaL1Bits != 0 && l2 == nil { // Should never happen if there's no L1.
		// 	return nil
		// }
	} else {
		// If there's an L1, then ri.l1() can be out of bounds but ri.l2() can't.
		// Windows 平台下，ri中表示第一维索引的值大于 2^6-1 了。
		if ri.l1() >= uint(len(mheap_.arenas)) {
			return nil
		}
	}
	// 至此，p表示的地址符合要求
	l2 := mheap_.arenas[ri.l1()]
	if arenaL1Bits != 0 && l2 == nil { // Should never happen if there's no L1.
		// Windows 平台
		return nil
	}
	// Linux平台l2绝对不会是nil。
	ha := l2[ri.l2()]
	if ha == nil {
		return nil
	}
	// (p/pageSize)%pagesPerArena p所在的页在ha所有页中的偏移量。
	return ha.spans[(p/pageSize)%pagesPerArena]
}
// spanOfUnchecked()，一个未验证地址p有效的 spanOf 版本。
// spanOfUnchecked is equivalent to spanOf, but the caller must ensure
// that p points into an allocated heap arena.
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOfUnchecked(p uintptr) *mspan {
	ai := arenaIndex(p)
	return mheap_.arenas[ai.l1()][ai.l2()].spans[(p/pageSize)%pagesPerArena]
}

// spanOfHeap is like spanOf, but returns nil if p does not point to a
// heap object.
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOfHeap(p uintptr) *mspan {
	s := spanOf(p)
	// s is nil if it's never been allocated. Otherwise, we check
	// its state first because we don't trust this pointer, so we
	// have to synchronize with span initialization. Then, it's
	// still possible we picked up a stale span pointer, so we
	// have to check the span's bounds.
	if s == nil || s.state.get() != mSpanInUse || p < s.base() || p >= s.limit {
		return nil
	}
	return s
}

// pageIndexOf returns the arena, page index, and page mask for pointer p.
// The caller must ensure p is in the heap.
func pageIndexOf(p uintptr) (arena *heapArena, pageIdx uintptr, pageMask uint8) {
	ai := arenaIndex(p)
	arena = mheap_.arenas[ai.l1()][ai.l2()]
	// p所在也对应 1024未的第几位，从0开始。
	// 除以8，因为8页对应一个字节
	pageIdx = ((p / pageSize) / 8) % uintptr(len(arena.pageInUse))
	// 这一位的掩码
	// (p/pageSize)%8 ，对应的位在一个字节中的偏移量，从0开始。
	pageMask = byte(1 << ((p / pageSize) % 8))
	return
}

// Initialize the heap.
func (h *mheap) init() {
	lockInit(&h.lock, lockRankMheap)
	lockInit(&h.speciallock, lockRankMheapSpecial)

	h.spanalloc.init(unsafe.Sizeof(mspan{}), recordspan, unsafe.Pointer(h), &memstats.mspan_sys)
	h.cachealloc.init(unsafe.Sizeof(mcache{}), nil, nil, &memstats.mcache_sys)
	h.specialfinalizeralloc.init(unsafe.Sizeof(specialfinalizer{}), nil, nil, &memstats.other_sys)
	h.specialprofilealloc.init(unsafe.Sizeof(specialprofile{}), nil, nil, &memstats.other_sys)
	h.specialReachableAlloc.init(unsafe.Sizeof(specialReachable{}), nil, nil, &memstats.other_sys)
	h.arenaHintAlloc.init(unsafe.Sizeof(arenaHint{}), nil, nil, &memstats.other_sys)

	// Don't zero mspan allocations. Background sweeping can
	// inspect a span concurrently with allocating it, so it's
	// important that the span's sweepgen survive across freeing
	// and re-allocating a span to prevent background sweeping
	// from improperly cas'ing it from 0.
	//
	// This is safe because mspan contains no heap pointers.
	h.spanalloc.zero = false

	// h->mapcache needs no init

	for i := range h.central {
		h.central[i].mcentral.init(spanClass(i))
	}

	h.pages.init(&h.lock, &memstats.gcMiscSys)
}

// reclaim sweeps and reclaims at least npage pages into the heap.
// It is called before allocating npage pages to keep growth in check.
//
// reclaim implements the page-reclaimer half of the sweeper.
//
// h.lock must NOT be held.
//
// npage 要scavenged的页数，不过最少需要scavenged 512页。
func (h *mheap) reclaim(npage uintptr) {
	// TODO(austin): Half of the time spent freeing spans is in
	// locking/unlocking the heap (even with low contention). We
	// could make the slow path here several times faster by
	// batching heap frees.

	// Bail early if there's no more reclaim work.
	if h.reclaimIndex.Load() >= 1<<63 {
		return
	}

	// Disable preemption so the GC can't start while we're
	// sweeping, so we can read h.sweepArenas, and so
	// traceGCSweepStart/Done pair on the P.
	mp := acquirem()

	if trace.enabled {
		traceGCSweepStart()
	}

	arenas := h.sweepArenas
	locked := false
	for npage > 0 {
		// Pull from accumulated credit first.
		if credit := h.reclaimCredit.Load(); credit > 0 {
			take := credit
			if take > npage {
				// Take only what we need.
				take = npage
			}
			if h.reclaimCredit.CompareAndSwap(credit, credit-take) {
				npage -= take
			}
			continue
		}

		// Claim a chunk of work.
		idx := uintptr(h.reclaimIndex.Add(pagesPerReclaimerChunk) - pagesPerReclaimerChunk)
		if idx/pagesPerArena >= uintptr(len(arenas)) {
			// Page reclaiming is done.
			h.reclaimIndex.Store(1 << 63)
			break
		}

		if !locked {
			// Lock the heap for reclaimChunk.
			lock(&h.lock)
			locked = true
		}

		// Scan this chunk.
		nfound := h.reclaimChunk(arenas, idx, pagesPerReclaimerChunk)
		if nfound <= npage {
			npage -= nfound
		} else {
			// Put spare pages toward global credit.
			h.reclaimCredit.Add(nfound - npage)
			npage = 0
		}
	}
	if locked {
		unlock(&h.lock)
	}

	if trace.enabled {
		traceGCSweepDone()
	}
	releasem(mp)
}

// reclaimChunk sweeps unmarked spans that start at page indexes [pageIdx, pageIdx+n).
// It returns the number of pages returned to the heap.
//
// h.lock must be held and the caller must be non-preemptible. Note: h.lock may be
// temporarily unlocked and re-locked in order to do sweeping or if tracing is
// enabled.
//
// GC Sweep阶段执行。清扫：即将在 heapArena.pageInUse 中对应位为1，在 heapArena.pageMarks
// 中对应位为1的msapn的
//
// 参数：
// arenas mheap.sweepArenas 的快照。
// pageIdx 从此页开始清扫。
// n 最有可能是512 要清扫的页数。
// 返回值：
// 已清扫的页数。
func (h *mheap) reclaimChunk(arenas []arenaIdx, pageIdx, n uintptr) uintptr {
	// The heap lock must be held because this accesses the
	// heapArena.spans arrays using potentially non-live pointers.
	// In particular, if a span were freed and merged concurrently
	// with this probing heapArena.spans, it would be possible to
	// observe arbitrary, stale span pointers.
	assertLockHeld(&h.lock)

	n0 := n
	var nFreed uintptr
	sl := sweep.active.begin()
	if !sl.valid {
		return 0
	}
	for n > 0 {
		ai := arenas[pageIdx/pagesPerArena]
		ha := h.arenas[ai.l1()][ai.l2()]

		// Get a chunk of the bitmap to work on.
		arenaPage := uint(pageIdx % pagesPerArena)
		inUse := ha.pageInUse[arenaPage/8:]
		marked := ha.pageMarks[arenaPage/8:]
		if uintptr(len(inUse)) > n/8 {
			inUse = inUse[:n/8]
			marked = marked[:n/8]
		}

		// Scan this bitmap chunk for spans that are in-use
		// but have no marked objects on them.
		for i := range inUse {
			inUseUnmarked := atomic.Load8(&inUse[i]) &^ marked[i]
			if inUseUnmarked == 0 {
				continue
			}

			for j := uint(0); j < 8; j++ {
				if inUseUnmarked&(1<<j) != 0 {
					s := ha.spans[arenaPage+uint(i)*8+j]
					if s, ok := sl.tryAcquire(s); ok {
						npages := s.npages
						unlock(&h.lock)
						if s.sweep(false) {
							nFreed += npages
						}
						lock(&h.lock)
						// Reload inUse. It's possible nearby
						// spans were freed when we dropped the
						// lock and we don't want to get stale
						// pointers from the spans array.
						inUseUnmarked = atomic.Load8(&inUse[i]) &^ marked[i]
					}
				}
			}
		}

		// Advance.
		pageIdx += uintptr(len(inUse) * 8)
		n -= uintptr(len(inUse) * 8)
	}
	sweep.active.end(sl)
	if trace.enabled {
		unlock(&h.lock)
		// Account for pages scanned but not reclaimed.
		traceGCSweepSpan((n0 - nFreed) * pageSize)
		lock(&h.lock)
	}

	assertLockHeld(&h.lock) // Must be locked on return.
	return nFreed
}

// spanAllocType represents the type of allocation to make, or
// the type of allocation to be freed.
type spanAllocType uint8

const (
	spanAllocHeap          spanAllocType = iota // heap span
	spanAllocStack                              // stack span
	spanAllocPtrScalarBits                      // unrolled GC prog bitmap span
	spanAllocWorkBuf                            // work buf span
)

// manual returns true if the span allocation is manually managed.
func (s spanAllocType) manual() bool {
	return s != spanAllocHeap
}

// alloc allocates a new span of npage pages from the GC'd heap.
//
// spanclass indicates the span's size class and scannability.
//
// Returns a span that has been fully initialized. span.needzero indicates
// whether the span has been zeroed. Note that it may not be.
func (h *mheap) alloc(npages uintptr, spanclass spanClass) *mspan {
	// Don't do any operations that lock the heap on the G stack.
	// It might trigger stack growth, and the stack growth code needs
	// to be able to allocate heap.
	var s *mspan
	systemstack(func() {
		// To prevent excessive heap growth, before allocating n pages
		// we need to sweep and reclaim at least n pages.
		if !isSweepDone() {
			h.reclaim(npages)
		}
		s = h.allocSpan(npages, spanAllocHeap, spanclass)
	})
	return s
}

// allocManual allocates a manually-managed span of npage pages.
// allocManual returns nil if allocation fails.
//
// allocManual adds the bytes used to *stat, which should be a
// memstats in-use field. Unlike allocations in the GC'd heap, the
// allocation does *not* count toward heapInUse.
//
// The memory backing the returned span may not be zeroed if
// span.needzero is set.
//
// allocManual must be called on the system stack because it may
// acquire the heap lock via allocSpan. See mheap for details.
//
// If new code is written to call allocManual, do NOT use an
// existing spanAllocType value and instead declare a new one.
//
//go:systemstack
func (h *mheap) allocManual(npages uintptr, typ spanAllocType) *mspan {
	if !typ.manual() {
		throw("manual span allocation called with non-manually-managed type")
	}
	return h.allocSpan(npages, typ, 0)
}
// setSpans()
// 将根据 base 找到对应的 arena 的元数据描述 heapArena，
// 然后将页和s的关系添加到 heapArena.spans 中。
// [base,base+npages*pageSize)可能跨越多个arena。
//
// setSpans modifies the span map so [spanOf(base), spanOf(base+npage*pageSize))
// is s.
// 用来把一个给定的span映射到相关的 heapArena 中。
func (h *mheap) setSpans(base, npage uintptr, s *mspan) {
	// p表示base表示的地址所在页的偏移量
	p := base / pageSize
	// ai 是 base 所在arena的索引。
	ai := arenaIndex(base)
	// 根据arena索引ai可以得到对应arena的元数据ha(heapArena类型)
	ha := h.arenas[ai.l1()][ai.l2()]
	for n := uintptr(0); n < npage; n++ {
		// i表示在一个arena中包含的所有页中的偏移量。
		i := (p + n) % pagesPerArena
		if i == 0 {
			//如果页的偏移量为0，表示i是某个arena中的第一页，
			// 如果n不是0，那就表示这个 msapn 跨arena了。
			// base+n*pageSize 为新的arena的起始地址(第一个页中的第一个字节的地址)。
			ai = arenaIndex(base + n*pageSize)
			// 根据新的编号获取heapArena的地址。
			ha = h.arenas[ai.l1()][ai.l2()]
		}
		// 将arena中页的偏移量映射到*mSpan
		ha.spans[i] = s
	}
}

// allocNeedsZero checks if the region of address space [base, base+npage*pageSize),
// assumed to be allocated, needs to be zeroed, updating heap arena metadata for
// future allocations.
//
// This must be called each time pages are allocated from the heap, even if the page
// allocator can otherwise prove the memory it's allocating is already zero because
// they're fresh from the operating system. It updates heapArena metadata that is
// critical for future page allocations.
//
// There are no locking constraints on this method.
//
// 检查 [base, base+npage*pageSize) 这段地址空间是否需要清零，并更其所在的arena.zeroedBase字段。
// mheap.sysAlloc
func (h *mheap) allocNeedsZero(base, npage uintptr) (needZero bool) {
	for npage > 0 {
		ai := arenaIndex(base)
		// ha在mheap.sysAlloc函数调用中进行分配，此函数只将其地址空间设置为reversed状态。
		ha := h.arenas[ai.l1()][ai.l2()]

		zeroedBase := atomic.Loaduintptr(&ha.zeroedBase)
		arenaBase := base % heapArenaBytes
		if arenaBase < zeroedBase {
			// We extended into the non-zeroed part of the
			// arena, so this region needs to be zeroed before use.
			//
			// zeroedBase is monotonically increasing, so if we see this now then
			// we can be sure we need to zero this memory region.
			//
			// We still need to update zeroedBase for this arena, and
			// potentially more arenas.
			needZero = true
		}
		// We may observe arenaBase > zeroedBase if we're racing with one or more
		// allocations which are acquiring memory directly before us in the address
		// space. But, because we know no one else is acquiring *this* memory, it's
		// still safe to not zero.

		// Compute how far into the arena we extend into, capped
		// at heapArenaBytes.
		arenaLimit := arenaBase + npage*pageSize
		if arenaLimit > heapArenaBytes {
			// 跨越了一个arena
			arenaLimit = heapArenaBytes
		}
		// Increase ha.zeroedBase so it's >= arenaLimit.
		// We may be racing with other updates.
		for arenaLimit > zeroedBase {
			// 原子更新 ha.zeroedBase
			if atomic.Casuintptr(&ha.zeroedBase, zeroedBase, arenaLimit) {
				break
			}
			zeroedBase = atomic.Loaduintptr(&ha.zeroedBase)
			// Double check basic conditions of zeroedBase.
			if zeroedBase <= arenaLimit && zeroedBase > arenaBase {
				// The zeroedBase moved into the space we were trying to
				// claim. That's very bad, and indicates someone allocated
				// the same region we did.
				throw("potentially overlapping in-use allocations detected")
			}
		}

		// Move base forward and subtract from npage to move into
		// the next arena, or finish.
		//
		// 跨arena了
		base += arenaLimit - arenaBase
		npage -= (arenaLimit - arenaBase) / pageSize
	}
	return
}

// tryAllocMSpan attempts to allocate an mspan object from
// the P-local cache, but may fail.
//
// h.lock need not be held.
//
// This caller must ensure that its P won't change underneath
// it during this function. Currently to ensure that we enforce
// that the function is run on the system stack, because that's
// the only place it is used now. In the future, this requirement
// may be relaxed if its use is necessary elsewhere.
//
// 从 p.mspancache 的 mspan 结构体缓存中分配一个 mspan 结构体(没有对应的内存页).
//
//go:systemstack
func (h *mheap) tryAllocMSpan() *mspan {
	pp := getg().m.p.ptr()
	// If we don't have a p or the cache is empty, we can't do
	// anything here.
	if pp == nil || pp.mspancache.len == 0 {
		return nil
	}
	// Pull off the last entry in the cache.
	s := pp.mspancache.buf[pp.mspancache.len-1]
	pp.mspancache.len--
	return s
}

// allocMSpanLocked allocates an mspan object.
//
// h.lock must be held.
//
// allocMSpanLocked must be called on the system stack because
// its caller holds the heap lock. See mheap for details.
// Running on the system stack also ensures that we won't
// switch Ps during this function. See tryAllocMSpan for details.
//
// 如果当前 p 不可用，直接使用 mheap.spanalloc() 返回一个 mspan 接头体对象。
// 否则如果 p.mspancache.len==0，则通过 mheap.spanalloc()填充64个 mspan 结构体，
// 到 p.mspancache ,并返回 p.mspancache[63]。
//
//go:systemstack
func (h *mheap) allocMSpanLocked() *mspan {
	assertLockHeld(&h.lock)

	pp := getg().m.p.ptr()
	if pp == nil {
		// We don't have a p so just do the normal thing.
		return (*mspan)(h.spanalloc.alloc())
	}
	// Refill the cache if necessary.
	if pp.mspancache.len == 0 {
		const refillCount = len(pp.mspancache.buf) / 2
		for i := 0; i < refillCount; i++ {
			pp.mspancache.buf[i] = (*mspan)(h.spanalloc.alloc())
		}
		pp.mspancache.len = refillCount
	}
	// Pull off the last entry in the cache.
	s := pp.mspancache.buf[pp.mspancache.len-1]
	pp.mspancache.len--
	return s
}

// freeMSpanLocked free an mspan object.
//
// h.lock must be held.
//
// freeMSpanLocked must be called on the system stack because
// its caller holds the heap lock. See mheap for details.
// Running on the system stack also ensures that we won't
// switch Ps during this function. See tryAllocMSpan for details.
//
// 释放一个 mspan 对象，如果当前 p.mspancache 缓存未满，则填充到其中。
// 否则调用 mheap.spanalloc.free 进行释放。
//
//go:systemstack
func (h *mheap) freeMSpanLocked(s *mspan) {
	assertLockHeld(&h.lock)

	pp := getg().m.p.ptr()
	// First try to free the mspan directly to the cache.
	if pp != nil && pp.mspancache.len < len(pp.mspancache.buf) {
		pp.mspancache.buf[pp.mspancache.len] = s
		pp.mspancache.len++
		return
	}
	// Failing that (or if we don't have a p), just free it to
	// the heap.
	h.spanalloc.free(unsafe.Pointer(s))
}

// allocSpan allocates an mspan which owns npages worth of memory.
//
// If typ.manual() == false, allocSpan allocates a heap span of class spanclass
// and updates heap accounting.
//
// If manual == true, allocSpan allocates a
// manually-managed span (spanclass is ignored), and the caller is
// responsible for any accounting related to its use of the span.
//
// Either way, allocSpan will atomically add the bytes in the newly allocated
// span to *sysStat.
//
// The returned span is fully initialized.
//
// h.lock must not be held.
//
// allocSpan must be called on the system stack both because it acquires
// the heap lock and because it must block GC transitions.
//
// span 分配逻辑。
// allocSpan()方法最主要的工作可以分成3步:
// 第一步分配一组内存页面
// 第二步分配 mspan 结构
// 第三步设置 heapArena 中的 heapArena.spans 映射。
//
// 内存页面和 mspan 都有特定的分配器。
//
// 从推中根据spanclass、npages和spanAllocType构建一个mspan，并为这个mspan
// 管理的内存分配空间。如果推中现存的处于prepared的页面不够，就从推中重新grow一些。然后再
// 通过 pageAlloc 分配。无论如何mspan管理的内存都是由 pageAlloc 进行分配的。
//
// 如果堆中没有空间了，就尝试通过 allocMSpanLocked 看有没有可供重用的且符合参数要求的mspan。
//
//go:systemstack
func (h *mheap) allocSpan(npages uintptr, typ spanAllocType, spanclass spanClass) (s *mspan) {
	// Function-global state.
	gp := getg()
	base, scav := uintptr(0), uintptr(0)
	growth := uintptr(0)

	// On some platforms we need to provide physical page aligned stack
	// allocations. Where the page size is less than the physical page
	// size, we already manage to do this by default.
	needPhysPageAlign := physPageAlignedStacks && typ == spanAllocStack && pageSize < physPageSize

	// If the allocation is small enough, try the page cache!
	// The page cache does not support aligned allocations, so we cannot use
	// it if we need to provide a physical page aligned stack allocation.
	pp := gp.m.p.ptr()
	// pageCachePages/4=64/4=16
	if !needPhysPageAlign && pp != nil && npages < pageCachePages/4 {
		c := &pp.pcache
		// If the cache is empty, refill it.
		if c.empty() {
			lock(&h.lock)
			*c = h.pages.allocToCache()
			unlock(&h.lock)
		}

		// Try to allocate from the cache.
		base, scav = c.alloc(npages)
		if base != 0 {
			s = h.tryAllocMSpan()
			if s != nil {
				goto HaveSpan
			}
			// We have a base but no mspan, so we need
			// to lock the heap.
		}
	}

	// For one reason or another, we couldn't get the
	// whole job done without the heap lock.
	lock(&h.lock)

	if needPhysPageAlign {
		// Overallocate by a physical page to allow for later alignment.
		extraPages := physPageSize / pageSize

		// Find a big enough region first, but then only allocate the
		// aligned portion. We can't just allocate and then free the
		// edges because we need to account for scavenged memory, and
		// that's difficult with alloc.
		//
		// Note that we skip updates to searchAddr here. It's OK if
		// it's stale and higher than normal; it'll operate correctly,
		// just come with a performance cost.
		base, _ = h.pages.find(npages + extraPages)
		if base == 0 {
			var ok bool
			growth, ok = h.grow(npages + extraPages)
			if !ok {
				unlock(&h.lock)
				return nil
			}
			base, _ = h.pages.find(npages + extraPages)
			if base == 0 {
				throw("grew heap, but no adequate free space found")
			}
		}
		base = alignUp(base, physPageSize)
		scav = h.pages.allocRange(base, npages)
	}

	if base == 0 {
		// Try to acquire a base address.
		// 尝试从 pageAlloc 中的inUse中分配页面。
		base, scav = h.pages.alloc(npages)
		if base == 0 {
			// h.pages中没有足够的页面来分配napages。
			var ok bool
			// 从堆中分配新的页面(将其地址区间变为prepared状态)，
			// 为h.pageAlloc的inUse添加新的addrRange，并针对addRange更新对应的位图和sum信息。
			growth, ok = h.grow(npages)
			// 因为npages要向上对齐到512(4MB)，所以growth肯定是4MB的倍数。
			if !ok {
				unlock(&h.lock)
				return nil
			}
			base, scav = h.pages.alloc(npages)
			// 添加了新的addrRange，所以再次通过h.pages分配。
			if base == 0 {
				throw("grew heap, but no adequate free space found")
			}
		}
	}
	if s == nil {
		// We failed to get an mspan earlier, so grab
		// one now that we have the heap lock.
		s = h.allocMSpanLocked()
	}
	unlock(&h.lock)

HaveSpan:
	// Decide if we need to scavenge in response to what we just allocated.
	// Specifically, we track the maximum amount of memory to scavenge of all
	// the alternatives below, assuming that the maximum satisfies *all*
	// conditions we check (e.g. if we need to scavenge X to satisfy the
	// memory limit and Y to satisfy heap-growth scavenging, and Y > X, then
	// it's fine to pick Y, because the memory limit is still satisfied).
	//
	// It's fine to do this after allocating because we expect any scavenged
	// pages not to get touched until we return. Simultaneously, it's important
	// to do this before calling sysUsed because that may commit address space.
	//
	// sync scavenge要scavenge的字节大小。
	bytesToScavenge := uintptr(0)
	//
	// println("go119MemoryLimitSupport",go119MemoryLimitSupport) // true
	// println("gcCPULimiter.limiting()",gcCPULimiter.limiting()) // false
	// println("limit",gcController.memoryLimit.Load()) // 0
	// println(gcController.memoryLimit.Load(),"gcController.memoryLimit.Load()")
	if limit := gcController.memoryLimit.Load(); go119MemoryLimitSupport && !gcCPULimiter.limiting() {
		// Assist with scavenging to maintain the memory limit by the amount
		// that we expect to page in.
		//
		// inuse 已经处于ready状态的字节数，通过sysUsed设置。
		inuse := gcController.mappedReady.Load()
		// Be careful about overflow, especially with uintptrs. Even on 32-bit platforms
		// someone can set a really big memory limit that isn't maxInt64.
		if uint64(scav)+inuse > uint64(limit) {
			bytesToScavenge = uintptr(uint64(scav) + inuse - uint64(limit))
		}
	}
	// println("bytesToScavenge1",bytesToScavenge)
	// bytesToScavenge1 6888464
	
	
	// grow 不为0表示npages立刻不能通过pageAlloc分配的，然后调用mheap.grow增加npages到pageAlloc的inUse中
	// ,再调用pageAlloc分配的。这个growth就是npages向上对齐到512页后的页数的总字节数。
	//
	// 即因为分配npages而分配了一些页并将这些页设置为prepared状态。
	if goal := scavenge.gcPercentGoal.Load(); goal != ^uint64(0) && growth > 0 {
		// We just caused a heap growth, so scavenge down what will soon be used.
		// By scavenging inline we deal with the failure to allocate out of
		// memory fragments by scavenging the memory fragments that are least
		// likely to be re-used.
		//
		// Only bother with this because we're not using a memory limit. We don't
		// care about heap growths as long as we're under the memory limit, and the
		// previous check for scaving already handles that.
		//
		// 没有设置GOMEMLIMIT
		if retained := heapRetained(); retained+uint64(growth) > goal {
			// The scavenging algorithm requires the heap lock to be dropped so it
			// can acquire it only sparingly. This is a potentially expensive operation
			// so it frees up other goroutines to allocate in the meanwhile. In fact,
			// they can make use of the growth we just created.
			todo := growth
			if overage := uintptr(retained + uint64(growth) - goal); todo > overage {
				todo = overage
			}
			if todo > bytesToScavenge {
				bytesToScavenge = todo
			}
			// bytesToScavenge为max(min(growth,overage),bytesToScavenge)
		}
	}
	// bytesToScavenge为memorylimit scavenge 和 gcPercent scavenge 计算到的较大值。
	// 如果没有竖着MEMORYLIMIT时大概率为：gcController.mappedReady.Load() + scav。
	//
	// println("bytesToScavenged",bytesToScavenge)
	// bytesToScavenged 6888464
	// bytesToScavenge为GOMEMLIMIT限制计算要scavenged的字节和GC限制计算要scavenged的字节中的较大值。
	//
	// There are a few very limited cirumstances where we won't have a P here.
	// It's OK to simply skip scavenging in these cases. Something else will notice
	// and pick up the tab.
	var now int64
	if pp != nil && bytesToScavenge > 0 {
		// 用于执行scavenge的时间是受限的。
		//
		// Measure how long we spent scavenging and add that measurement to the assist
		// time so we can track it for the GC CPU limiter.
		//
		// Limiter event tracking might be disabled if we end up here
		// while on a mark worker.
		//
		// 记录此次scavenge执行的时间。
		start := nanotime()
		track := pp.limiterEvent.start(limiterEventScavengeAssist, start)

		// Scavenge, but back out if the limiter turns on.
		h.pages.scavenge(bytesToScavenge, func() bool {
			return gcCPULimiter.limiting()
		})

		// Finish up accounting.
		now = nanotime()
		if track {
			pp.limiterEvent.stop(limiterEventScavengeAssist, now)
		}
		// 记录此次scavenge执行的时间。
		scavenge.assistTime.Add(now - start)
	}

	// Initialize the span.
	// 根据spanclass/typ实例化初始s，并将s添加到其对应的heapArena的spans中。
	h.initSpan(s, typ, spanclass, base, npages)

	// Commit and account for any scavenged memory that the span now owns.
	nbytes := npages * pageSize
	if scav != 0 {
		// sysUsed all the pages that are actually available
		// in the span since some of them might be scavenged.
		//
		// 如果npages中包含有被scavenged的页，就将所有页设置为ready状态。
		sysUsed(unsafe.Pointer(base), nbytes, scav)
		// scavenged状态的页其对应的 pageAlloc.chunks中的scavenged位已经在前面
		// 设置为0了且这些pages也变成了reay状态，所以要从gcController.heapReleased
		// 计数中删除。
		gcController.heapReleased.add(-int64(scav))
	}
	// Update stats.
	// 如果这些页面中除了scavenged外还包含空闲页(被用于msapn后又回收了的页，即
	// pageAlloc.chunks 中alooc位和scavenged位都为0的页)，
	// 将其对应的字节数从gcController.heapFree统计计数中去除。
	gcController.heapFree.add(-int64(nbytes - scav))
	if typ == spanAllocHeap {
		gcController.heapInUse.add(int64(nbytes))
	}
	// Update consistent stats.
	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.committed, int64(scav))
	atomic.Xaddint64(&stats.released, -int64(scav))
	switch typ {
	case spanAllocHeap:
		atomic.Xaddint64(&stats.inHeap, int64(nbytes))
	case spanAllocStack:
		atomic.Xaddint64(&stats.inStacks, int64(nbytes))
	case spanAllocPtrScalarBits:
		atomic.Xaddint64(&stats.inPtrScalarBits, int64(nbytes))
	case spanAllocWorkBuf:
		atomic.Xaddint64(&stats.inWorkBufs, int64(nbytes))
	}
	memstats.heapStats.release()

	pageTraceAlloc(pp, now, base, npages)
	return s
}

// initSpan initializes a blank span s which will represent the range
// [base, base+npages*pageSize). typ is the type of span being allocated.
//
// 根据typ和spanclass设置s上的某些字段，并将s添加到其对应的 heapArena.spans 中。
// [base,base_npages*pagesize)可能跨越多个arena。
//
// 参数：
// s 一个零值mspan的地址
// typ mspan 类型。
// spanClass [0,136)
// base npages的首页起始地址。
// npages s占用的页数。
func (h *mheap) initSpan(s *mspan, typ spanAllocType, spanclass spanClass, base, npages uintptr) {
	// At this point, both s != nil and base != 0, and the heap
	// lock is no longer held. Initialize the span.
	s.init(base, npages)
	if h.allocNeedsZero(base, npages) {
		s.needzero = 1
	}
	nbytes := npages * pageSize
	if typ.manual() {
		// !=spanAllocHeap
		s.manualFreeList = 0
		s.nelems = 0
		s.limit = s.base() + s.npages*pageSize
		s.state.set(mSpanManual)
	} else {
		// We must set span properties before the span is published anywhere
		// since we're not holding the heap lock.
		s.spanclass = spanclass
		if sizeclass := spanclass.sizeclass(); sizeclass == 0 {
			s.elemsize = nbytes
			s.nelems = 1
			s.divMul = 0
		} else {
			s.elemsize = uintptr(class_to_size[sizeclass])
			s.nelems = nbytes / s.elemsize
			s.divMul = class_to_divmagic[sizeclass]
		}

		// Initialize mark and allocation structures.
		s.freeindex = 0
		s.freeIndexForScan = 0
		s.allocCache = ^uint64(0) // all 1s indicating all free.
		s.gcmarkBits = newMarkBits(s.nelems)
		s.allocBits = newAllocBits(s.nelems)

		// It's safe to access h.sweepgen without the heap lock because it's
		// only ever updated with the world stopped and we run on the
		// systemstack which blocks a STW transition.
		atomic.Store(&s.sweepgen, h.sweepgen)

		// Now that the span is filled in, set its state. This
		// is a publication barrier for the other fields in
		// the span. While valid pointers into this span
		// should never be visible until the span is returned,
		// if the garbage collector finds an invalid pointer,
		// access to the span may race with initialization of
		// the span. We resolve this race by atomically
		// setting the state after the span is fully
		// initialized, and atomically checking the state in
		// any situation where a pointer is suspect.
		s.state.set(mSpanInUse)
	}

	// Publish the span in various locations.

	// This is safe to call without the lock held because the slots
	// related to this span will only ever be read or modified by
	// this thread until pointers into the span are published (and
	// we execute a publication barrier at the end of this function
	// before that happens) or pageInUse is updated.
	//
	// 将s添加到对应的heapArena.spans中
	h.setSpans(s.base(), npages, s)

	if !typ.manual() {
		// Mark in-use span in arena page bitmap.
		//
		// This publishes the span to the page sweeper, so
		// it's imperative that the span be completely initialized
		// prior to this line.
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		atomic.Or8(&arena.pageInUse[pageIdx], pageMask)

		// Update related page sweeper stats.
		h.pagesInUse.Add(npages)
	}

	// Make sure the newly allocated span will be observed
	// by the GC before pointers into the span are published.
	publicationBarrier()
}

// Try to add at least npage pages of memory to the heap,
// returning how much the heap grew by and whether it worked.
//
// h.lock must be held.
//
// 从 mheap.curArena 表示的arena中分配npages(向上对齐到512页,4MB)，
// 其实是将npages(对齐后的)这段地址空间标记为prepared状态。并更新对应的元数据信息。
// 如果mheap.curArena不够分配npages，则会从堆中分配(将其对应的地址空间标记为reversed状态)
// 新的arenas，并更新mheap.curArena。这些页在真正使用时(比如划分为mspan)才会将其地址区间
// 变为ready状态。
//
// 首先将npage包含的字节数向上对齐到4MB(chunk对应的页(512页)总字节数)。
// 然后将这段地址空间从reversed状态变为prepared(调用sysmap)。
//
// 如果 mheap.curArena 剩余地址空间小于不够分配npage，就会从调用 mheap.sysAlloc
// 从堆中分配新的 arena (64MB amd64)，如果npage大于一个arena的大小，可能会分配多个arena，
// 并将这些arena对应的地址空间标记为reversed状态。如果新分配的arenas(可能多个相连)首个arena
// 的起始地址没有与mheap.curArena.end相连，则将mheap.curArena剩余空间通过调用pageAlloc.grow
// 更新为prepared状态。将mheap.curArena更新为刚刚分配的arenas首尾个arena的起始地址和最后一个
// arena的末端地址。
// 如果相连，则将mheap.curArena.end 设置为arenas末端arena的地址空间的末尾地址。
//
// 此函数返回之后，这段因npage ready的分配的地址空间(向上对齐到512页,4MB)，就可以直接使用(比如用于分配mspan)了。
//
// 返回值：
// uintptr: 4MB的整数倍，表示因npages而prepared的地址空间大小。
// bool 表示是否成功。如果为false则第一个返回值没有意义。
func (h *mheap) grow(npage uintptr) (uintptr, bool) {
	assertLockHeld(&h.lock)

	// We must grow the heap in whole palloc chunks.
	// We call sysMap below but note that because we
	// round up to pallocChunkPages which is on the order
	// of MiB (generally >= to the huge page size) we
	// won't be calling it too much.
	//
	// ask=4MB
	ask := alignUp(npage, pallocChunkPages) * pageSize

	totalGrowth := uintptr(0)
	// This may overflow because ask could be very large
	// and is otherwise unrelated to h.curArena.base.
	end := h.curArena.base + ask
	nBase := alignUp(end, physPageSize)
	// 此函数第一次调用时 h.curArena.end=h.curArena.base=0
	//
	// nBase > h.curArena.end 第一次调用时会出现。因为此时h.curArena.end还是0
	// end < h.curArena.base  当前h.curArena剩余地址空间不够分配ask指定的空间。
	if nBase > h.curArena.end || /* overflow */ end < h.curArena.base {
		// Not enough room in the current arena. Allocate more
		// arena space. This may not be contiguous with the
		// current arena, so we have to request the full ask.
		//
		// 一次只是分配一个arena(64MB)的空间(只是进行了保留处理)，并不能直接使用。
		// 并将分配的arean注册到mehap_.allArenas中。
		// ask在sysAlloc中会向上对齐到64MB。
		av, asize := h.sysAlloc(ask, &h.arenaHints, true)
		// 第一次调用: av=0xC000000000
		// println("av",uintptr(av)==0xc000000000) // true
		// println(h.arenaHints.addr) h.arenaHints.addr=0xc000000000+64
		if av == nil {
			inUse := gcController.heapFree.load() + gcController.heapReleased.load() + gcController.heapInUse.load()
			print("runtime: out of memory: cannot allocate ", ask, "-byte block (", inUse, " in use)\n")
			return 0, false
		}

		if uintptr(av) == h.curArena.end {
			// The new space is contiguous with the old
			// space, so just extend the current space.
			h.curArena.end = uintptr(av) + asize
		} else {
			// 新的space没有连接着当前的h.curArean。
			// The new space is discontiguous. Track what
			// remains of the current space and switch to
			// the new space. This should be rare.
			//
			// 第一次调用size=0
			// println("size:",h.curArena.end - h.curArena.base)
			if size := h.curArena.end - h.curArena.base; size != 0 {
				// 上面h.sysAlloc分配的地址区间没有连接着当前的h.curArena。
				// 将这段区间更新为prepared状态，并更新对应的元数据信息以供接下来使用。
				//
				// Transition this space from Reserved to Prepared and mark it
				// as released since we'll be able to start using it after updating
				// the page allocator and releasing the lock at any time.
				//
				// 对这段区间进行map处理，变为prepared状态。
				sysMap(unsafe.Pointer(h.curArena.base), size, &gcController.heapReleased)
				// Update stats.
				stats := memstats.heapStats.acquire()
				atomic.Xaddint64(&stats.released, int64(size))
				memstats.heapStats.release()
				// Update the page allocator's structures to make this
				// space ready for allocation.
				//
				//
				h.pages.grow(h.curArena.base, size)
				totalGrowth += size
			}
			// Switch to the new space.
			//
			// 一次分配的arenas(上面h.sysAlloc()调用中创建的arenas(至少1个arena))
			// 的起始地址av和asize(64MB的整数倍)。
			// 这段区间目前只是保留状态。
			h.curArena.base = uintptr(av)
			h.curArena.end = uintptr(av) + asize
		}

		// Recalculate nBase.
		// We know this won't overflow, because sysAlloc returned
		// a valid region starting at h.curArena.base which is at
		// least ask bytes in size.
		//
		// ask是4MB(一个chunk对应的地址空间的大小)的整数倍。
		nBase = alignUp(h.curArena.base+ask, physPageSize)
	}

	// Grow into the current arena.
	v := h.curArena.base
	h.curArena.base = nBase

	// Transition the space we're going to use from Reserved to Prepared.
	//
	// The allocation is always aligned to the heap arena
	// size which is always > physPageSize, so its safe to
	// just add directly to heapReleased.
	// println("mheap.grow mheap.grow",nBase-v)
	// 将nBase-v=ask大小的字节变为ready状态。
	sysMap(unsafe.Pointer(v), nBase-v, &gcController.heapReleased)
	// The memory just allocated counts as both released
	// and idle, even though it's not yet backed by spans.
	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.released, int64(nBase-v))
	memstats.heapStats.release()

	// Update the page allocator's structures to make this
	// space ready for allocation.
	//
	// 更新页面所在chunk的元数据信息。
	// nBase-v的值是4MB(一个chunk对应的页的字节大小)的整数倍。
	h.pages.grow(v, nBase-v)
	totalGrowth += nBase - v
	return totalGrowth, true
}

// Free the span back into the heap.
//
// 参数：
// s 一般在 sweepLocked.sweep 此s之后，如果其所有内存块都未被标记便会调用此函数，
// 将s管理的内存对应的地址区间在 pageAlloc.chunks 中的对应的alloc位/scavenged位
// 分别置为0和0(其实scavenged位本来就是0)。并更新在 pageAlloc.summary 中对应的各个层级的sum信息。
// 并未对此地址空间执行 sysUnused 调用。
func (h *mheap) freeSpan(s *mspan) {
	systemstack(func() {
		pageTraceFree(getg().m.p.ptr(), 0, s.base(), s.npages)

		lock(&h.lock)
		if msanenabled {
			// Tell msan that this entire span is no longer in use.
			base := unsafe.Pointer(s.base())
			bytes := s.npages << _PageShift
			msanfree(base, bytes)
		}
		if asanenabled {
			// Tell asan that this entire span is no longer in use.
			base := unsafe.Pointer(s.base())
			bytes := s.npages << _PageShift
			asanpoison(base, bytes)
		}
		h.freeSpanLocked(s, spanAllocHeap)
		unlock(&h.lock)
	})
}

// freeManual frees a manually-managed span returned by allocManual.
// typ must be the same as the spanAllocType passed to the allocManual that
// allocated s.
//
// This must only be called when gcphase == _GCoff. See mSpanState for
// an explanation.
//
// freeManual must be called on the system stack because it acquires
// the heap lock. See mheap for details.
//
//go:systemstack
func (h *mheap) freeManual(s *mspan, typ spanAllocType) {
	pageTraceFree(getg().m.p.ptr(), 0, s.base(), s.npages)

	s.needzero = 1
	lock(&h.lock)
	h.freeSpanLocked(s, typ)
	unlock(&h.lock)
}

func (h *mheap) freeSpanLocked(s *mspan, typ spanAllocType) {
	assertLockHeld(&h.lock)

	switch s.state.get() {
	case mSpanManual:
		if s.allocCount != 0 {
			throw("mheap.freeSpanLocked - invalid stack free")
		}
	case mSpanInUse:
		if s.isUserArenaChunk {
			throw("mheap.freeSpanLocked - invalid free of user arena chunk")
		}
		if s.allocCount != 0 || s.sweepgen != h.sweepgen {
			print("mheap.freeSpanLocked - span ", s, " ptr ", hex(s.base()), " allocCount ", s.allocCount, " sweepgen ", s.sweepgen, "/", h.sweepgen, "\n")
			throw("mheap.freeSpanLocked - invalid free")
		}
		h.pagesInUse.Add(-s.npages)

		// Clear in-use bit in arena page bitmap.
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		atomic.And8(&arena.pageInUse[pageIdx], ^pageMask)
	default:
		throw("mheap.freeSpanLocked - invalid span state")
	}

	// Update stats.
	//
	// Mirrors the code in allocSpan.
	nbytes := s.npages * pageSize
	gcController.heapFree.add(int64(nbytes))
	if typ == spanAllocHeap {
		gcController.heapInUse.add(-int64(nbytes))
	}
	// Update consistent stats.
	stats := memstats.heapStats.acquire()
	switch typ {
	case spanAllocHeap:
		atomic.Xaddint64(&stats.inHeap, -int64(nbytes))
	case spanAllocStack:
		atomic.Xaddint64(&stats.inStacks, -int64(nbytes))
	case spanAllocPtrScalarBits:
		atomic.Xaddint64(&stats.inPtrScalarBits, -int64(nbytes))
	case spanAllocWorkBuf:
		atomic.Xaddint64(&stats.inWorkBufs, -int64(nbytes))
	}
	memstats.heapStats.release()

	// Mark the space as free.
	h.pages.free(s.base(), s.npages, false)

	// Free the span structure. We no longer have a use for it.
	s.state.set(mSpanDead)
	h.freeMSpanLocked(s)
}

// scavengeAll acquires the heap lock (blocking any additional
// manipulation of the page allocator) and iterates over the whole
// heap, scavenging every free page available.
func (h *mheap) scavengeAll() {
	// Disallow malloc or panic while holding the heap lock. We do
	// this here because this is a non-mallocgc entry-point to
	// the mheap API.
	gp := getg()
	gp.m.mallocing++

	released := h.pages.scavenge(^uintptr(0), nil)

	gp.m.mallocing--

	if debug.scavtrace > 0 {
		printScavTrace(released, true)
	}
}

//go:linkname runtime_debug_freeOSMemory runtime/debug.freeOSMemory
func runtime_debug_freeOSMemory() {
	GC()
	systemstack(func() { mheap_.scavengeAll() })
}

// Initialize a new span with the given start and npages.
func (span *mspan) init(base uintptr, npages uintptr) {
	// span is *not* zeroed.
	span.next = nil
	span.prev = nil
	span.list = nil
	span.startAddr = base
	span.npages = npages
	span.allocCount = 0
	span.spanclass = 0
	span.elemsize = 0
	span.speciallock.key = 0
	span.specials = nil
	span.needzero = 0
	span.freeindex = 0
	span.freeIndexForScan = 0
	span.allocBits = nil
	span.gcmarkBits = nil
	span.state.set(mSpanDead)
	lockInit(&span.speciallock, lockRankMspanSpecial)
}

func (span *mspan) inList() bool {
	return span.list != nil
}

// Initialize an empty doubly-linked list.
func (list *mSpanList) init() {
	list.first = nil
	list.last = nil
}

func (list *mSpanList) remove(span *mspan) {
	if span.list != list {
		print("runtime: failed mSpanList.remove span.npages=", span.npages,
			" span=", span, " prev=", span.prev, " span.list=", span.list, " list=", list, "\n")
		throw("mSpanList.remove")
	}
	if list.first == span {
		list.first = span.next
	} else {
		span.prev.next = span.next
	}
	if list.last == span {
		list.last = span.prev
	} else {
		span.next.prev = span.prev
	}
	span.next = nil
	span.prev = nil
	span.list = nil
}

func (list *mSpanList) isEmpty() bool {
	return list.first == nil
}
// 向span插入到链表list头部。
func (list *mSpanList) insert(span *mspan) {
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insert", span, span.next, span.prev, span.list)
		throw("mSpanList.insert")
	}
	span.next = list.first
	if list.first != nil {
		// The list contains at least one span; link it in.
		// The last span in the list doesn't change.
		list.first.prev = span
	} else {
		// The list contains no spans, so this is also the last span.
		list.last = span
	}
	list.first = span
	span.list = list
}

func (list *mSpanList) insertBack(span *mspan) {
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insertBack", span, span.next, span.prev, span.list)
		throw("mSpanList.insertBack")
	}
	span.prev = list.last
	if list.last != nil {
		// The list contains at least one span.
		list.last.next = span
	} else {
		// The list contains no spans, so this is also the first span.
		list.first = span
	}
	list.last = span
	span.list = list
}

// takeAll removes all spans from other and inserts them at the front
// of list.
func (list *mSpanList) takeAll(other *mSpanList) {
	if other.isEmpty() {
		return
	}

	// Reparent everything in other to list.
	for s := other.first; s != nil; s = s.next {
		s.list = list
	}

	// Concatenate the lists.
	if list.isEmpty() {
		*list = *other
	} else {
		// Neither list is empty. Put other before list.
		other.last.next = list.first
		list.first.prev = other.last
		list.first = other.first
	}

	other.first, other.last = nil, nil
}

const (
	_KindSpecialFinalizer = 1
	_KindSpecialProfile   = 2
	// _KindSpecialReachable is a special used for tracking
	// reachability during testing.
	_KindSpecialReachable = 3
	// Note: The finalizer special must be first because if we're freeing
	// an object, a finalizer special will cause the freeing operation
	// to abort, and we want to keep the other special records around
	// if that happens.
)

type special struct {
	_      sys.NotInHeap
	// 链接下一个 special ，链表的头是 mspan.specials。
	next   *special // linked list in span
	// 关联 mspan 中的 object 在mspan中的字节偏移量。
	// 因为一个 mspan 的所有 special 都被串联在一起，
	// 此值用来识别special具体归属于哪个object。
	//
	// 关联此special的对象(或者对象的子部分，当此对象被用于p的tinyOb时)的起始地址
	// 相对于msapn管理的内存的起始地址偏移量。
	offset uint16   // span offset of object
	// special 类型：
	// 	_KindSpecialFinalizer = 1
	//	_KindSpecialProfile   = 2
	//	_KindSpecialReachable = 3 // 暂未发现被用到，有处理逻辑但没有案例。
	kind   byte     // kind of special
}

// spanHasSpecials marks a span as having specials in the arena bitmap.
//
// 将 mspan 所在的页中的第一页对应的二进制位(pageSpecials bitmap)设置为1。
func spanHasSpecials(s *mspan) {
	arenaPage := (s.base() / pageSize) % pagesPerArena
	ai := arenaIndex(s.base())
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	atomic.Or8(&ha.pageSpecials[arenaPage/8], uint8(1)<<(arenaPage%8))
}

// spanHasNoSpecials marks a span as having no specials in the arena bitmap.
func spanHasNoSpecials(s *mspan) {
	arenaPage := (s.base() / pageSize) % pagesPerArena
	ai := arenaIndex(s.base())
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	atomic.And8(&ha.pageSpecials[arenaPage/8], ^(uint8(1) << (arenaPage % 8)))
}

// Adds the special record s to the list of special records for
// the object p. All fields of s should be filled in except for
// offset & next, which this routine will fill in.
// Returns true if the special was successfully added, false otherwise.
// (The add will fail only if a record with the same p and s->kind
// already exists.)
func addspecial(p unsafe.Pointer, s *special) bool {
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("addspecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - span.base()
	kind := s.kind

	lock(&span.speciallock)

	// Find splice point, check for existing record.
	t := &span.specials
	for {
		x := *t
		if x == nil {
			break
		}
		if offset == uintptr(x.offset) && kind == x.kind {
			unlock(&span.speciallock)
			releasem(mp)
			return false // already exists
		}
		if offset < uintptr(x.offset) || (offset == uintptr(x.offset) && kind < x.kind) {
			break
		}
		t = &x.next
	}

	// Splice in record, fill in offset.
	s.offset = uint16(offset)
	s.next = *t
	*t = s
	spanHasSpecials(span)
	unlock(&span.speciallock)
	releasem(mp)

	return true
}

// Removes the Special record of the given kind for the object p.
// Returns the record if the record existed, nil otherwise.
// The caller must FixAlloc_Free the result.
func removespecial(p unsafe.Pointer, kind uint8) *special {
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("removespecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - span.base()

	var result *special
	lock(&span.speciallock)
	t := &span.specials
	for {
		s := *t
		if s == nil {
			break
		}
		// This function is used for finalizers only, so we don't check for
		// "interior" specials (p must be exactly equal to s->offset).
		if offset == uintptr(s.offset) && kind == s.kind {
			*t = s.next
			result = s
			break
		}
		t = &s.next
	}
	if span.specials == nil {
		spanHasNoSpecials(span)
	}
	unlock(&span.speciallock)
	releasem(mp)
	return result
}

// The described object has a finalizer set for it.
//
// specialfinalizer is allocated from non-GC'd memory, so any heap
// pointers must be specially handled.
type specialfinalizer struct {
	_       sys.NotInHeap
	special special
	fn      *funcval // May be a heap pointer.
	// finaliezer 函数返回值占的帧大小(对齐过的)。
	nret    uintptr
	// finalizer函数的入参元数据类型。
	fint    *_type   // May be a heap pointer, but always live.
	// Runtime.SetFinalizer 调用的第一个参数的值。
	ot      *ptrtype // May be a heap pointer, but always live.
}

// Adds a finalizer to the object p. Returns true if it succeeded.
//
// 参数：
// p runtime.SetFinalizer 的第一个参数转换为 unsafe.Pointer
// f runtime.SetFinalizer 的第二个参数转换为 funcval
// nret 第二个参数的返回值占用的空间。
// fint 第二个参数-函数的第一个参数的类型元数据。
// ot   第一个参数的元数据类型。
func addfinalizer(p unsafe.Pointer, f *funcval, nret uintptr, fint *_type, ot *ptrtype) bool {
	lock(&mheap_.speciallock)
	s := (*specialfinalizer)(mheap_.specialfinalizeralloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialFinalizer
	s.fn = f
	s.nret = nret
	s.fint = fint
	s.ot = ot
	// s的地址与s.special地址相同，而s.special的地址值又存储在 mspan.specials 链。
	// 因为 special 类型包括几种，使用一种special结构体表示它们共有的。
	// 需要具体类型时，将其转换为具体指针即可。
	// s 是 *special 类型。
	// 比如 (*specialfinalizer)(s)
	if addspecial(p, &s.special) {
		// This is responsible for maintaining the same
		// GC-related invariants as markrootSpans in any
		// situation where it's possible that markrootSpans
		// has already run but mark termination hasn't yet.
		if gcphase != _GCoff {
			base, span, _ := findObject(uintptr(p), 0, 0)
			mp := acquirem()
			gcw := &mp.p.ptr().gcw
			// Mark everything reachable from the object
			// so it's retained for the finalizer.
			if !span.spanclass.noscan() {
				scanobject(base, gcw)
			}
			// Mark the finalizer itself, since the
			// special isn't part of the GC'd heap.
			scanblock(uintptr(unsafe.Pointer(&s.fn)), goarch.PtrSize, &oneptrmask[0], gcw, nil)
			releasem(mp)
		}
		return true
	}

	// There was an old finalizer
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
	return false
}

// Removes the finalizer (if any) from the object p.
func removefinalizer(p unsafe.Pointer) {
	s := (*specialfinalizer)(unsafe.Pointer(removespecial(p, _KindSpecialFinalizer)))
	if s == nil {
		return // there wasn't a finalizer to remove
	}
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
}

// The described object is being heap profiled.
type specialprofile struct {
	_       sys.NotInHeap
	special special
	b       *bucket
}

// Set the heap profile bucket associated with addr to b.
func setprofilebucket(p unsafe.Pointer, b *bucket) {
	lock(&mheap_.speciallock)
	s := (*specialprofile)(mheap_.specialprofilealloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialProfile
	s.b = b
	if !addspecial(p, &s.special) {
		throw("setprofilebucket: profile already set")
	}
}

// specialReachable tracks whether an object is reachable on the next
// GC cycle. This is used by testing.
type specialReachable struct {
	special   special
	done      bool
	reachable bool
}

// specialsIter helps iterate over specials lists.
type specialsIter struct {
	pprev **special
	s     *special
}

func newSpecialsIter(span *mspan) specialsIter {
	return specialsIter{&span.specials, span.specials}
}

func (i *specialsIter) valid() bool {
	return i.s != nil
}

func (i *specialsIter) next() {
	i.pprev = &i.s.next
	i.s = *i.pprev
}

// unlinkAndNext removes the current special from the list and moves
// the iterator to the next special. It returns the unlinked special.
func (i *specialsIter) unlinkAndNext() *special {
	cur := i.s
	i.s = cur.next
	*i.pprev = i.s
	return cur
}

// freeSpecial performs any cleanup on special s and deallocates it.
// s must already be unlinked from the specials list.
func freeSpecial(s *special, p unsafe.Pointer, size uintptr) {
	switch s.kind {
	case _KindSpecialFinalizer:
		sf := (*specialfinalizer)(unsafe.Pointer(s))
		// 入队。
		queuefinalizer(p, sf.fn, sf.nret, sf.fint, sf.ot)
		lock(&mheap_.speciallock)
		// 释放 sf 结构体。因为 finalizer 回调中不需要。
		mheap_.specialfinalizeralloc.free(unsafe.Pointer(sf))
		unlock(&mheap_.speciallock)
	case _KindSpecialProfile:
		sp := (*specialprofile)(unsafe.Pointer(s))
		mProf_Free(sp.b, size)
		lock(&mheap_.speciallock)
		mheap_.specialprofilealloc.free(unsafe.Pointer(sp))
		unlock(&mheap_.speciallock)
	case _KindSpecialReachable:
		sp := (*specialReachable)(unsafe.Pointer(s))
		sp.done = true
		// The creator frees these.
	default:
		throw("bad special kind")
		panic("not reached")
	}
}

// gcBits is an alloc/mark bitmap. This is always used as gcBits.x.
type gcBits struct {
	_ sys.NotInHeap
	x uint8
}

// bytep returns a pointer to the n'th byte of b.
func (b *gcBits) bytep(n uintptr) *uint8 {
	return addb(&b.x, n)
}

// bitp returns a pointer to the byte containing bit n and a mask for
// selecting that bit from *bytep.
//
// bytep 为从b开始第n个bit所在uint8的起始地址，n从0开始。
// 这个bit在 bytep 所指的uint8中的位置为m(从0开始)，则 mask为2^m。
//
// 用 mask&(*bytep)!=0 可以确定从b开始第n个bit上是1，否则为0。
func (b *gcBits) bitp(n uintptr) (bytep *uint8, mask uint8) {
	return b.bytep(n / 8), 1 << (n % 8)
}

const gcBitsChunkBytes = uintptr(64 << 10)
const gcBitsHeaderBytes = unsafe.Sizeof(gcBitsHeader{})

type gcBitsHeader struct {
	free uintptr // free is the index into bits of the next free byte.
	next uintptr // *gcBits triggers recursive type bug. (issue 14620)
}

type gcBitsArena struct {
	_ sys.NotInHeap
	// gcBitsHeader // side step recursive type bug (issue 14620) by including fields by hand.
	free uintptr // free is the index into bits of the next free byte; read/write atomically
	next *gcBitsArena
	bits [gcBitsChunkBytes - gcBitsHeaderBytes]gcBits
}

// 见 nextMarkBitArenaEpoch 函数注释。
var gcBitsArenas struct {
	lock     mutex
	free     *gcBitsArena
	next     *gcBitsArena // Read atomically. Write atomically under lock.
	current  *gcBitsArena
	previous *gcBitsArena
}

// tryAlloc allocates from b or returns nil if b does not have enough room.
// This is safe to call concurrently.
func (b *gcBitsArena) tryAlloc(bytes uintptr) *gcBits {
	if b == nil || atomic.Loaduintptr(&b.free)+bytes > uintptr(len(b.bits)) {
		return nil
	}
	// Try to allocate from this block.
	end := atomic.Xadduintptr(&b.free, bytes)
	if end > uintptr(len(b.bits)) {
		return nil
	}
	// There was enough room.
	start := end - bytes
	return &b.bits[start]
}

// newMarkBits returns a pointer to 8 byte aligned bytes
// to be used for a span's mark bits.
func newMarkBits(nelems uintptr) *gcBits {
	blocksNeeded := uintptr((nelems + 63) / 64)
	bytesNeeded := blocksNeeded * 8

	// Try directly allocating from the current head arena.
	head := (*gcBitsArena)(atomic.Loadp(unsafe.Pointer(&gcBitsArenas.next)))
	if p := head.tryAlloc(bytesNeeded); p != nil {
		return p
	}

	// There's not enough room in the head arena. We may need to
	// allocate a new arena.
	lock(&gcBitsArenas.lock)
	// Try the head arena again, since it may have changed. Now
	// that we hold the lock, the list head can't change, but its
	// free position still can.
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate a new arena. This may temporarily drop the lock.
	fresh := newArenaMayUnlock()
	// If newArenaMayUnlock dropped the lock, another thread may
	// have put a fresh arena on the "next" list. Try allocating
	// from next again.
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		// Put fresh back on the free list.
		// TODO: Mark it "already zeroed"
		fresh.next = gcBitsArenas.free
		gcBitsArenas.free = fresh
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate from the fresh arena. We haven't linked it in yet, so
	// this cannot race and is guaranteed to succeed.
	p := fresh.tryAlloc(bytesNeeded)
	if p == nil {
		throw("markBits overflow")
	}

	// Add the fresh arena to the "next" list.
	fresh.next = gcBitsArenas.next
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), unsafe.Pointer(fresh))

	unlock(&gcBitsArenas.lock)
	return p
}

// newAllocBits returns a pointer to 8 byte aligned bytes
// to be used for this span's alloc bits.
// newAllocBits is used to provide newly initialized spans
// allocation bits. For spans not being initialized the
// mark bits are repurposed as allocation bits when
// the span is swept.
func newAllocBits(nelems uintptr) *gcBits {
	return newMarkBits(nelems)
}

// nextMarkBitArenaEpoch establishes a new epoch for the arenas
// holding the mark bits. The arenas are named relative to the
// current GC cycle which is demarcated by the call to finishweep_m.
//
// All current spans have been swept.
// During that sweep each span allocated room for its gcmarkBits in
// gcBitsArenas.next block.
//
// gcBitsArenas.next becomes the gcBitsArenas.current
// where the GC will mark objects and after each span is swept these bits
// will be used to allocate objects.
//
// gcBitsArenas.current becomes gcBitsArenas.previous where the span's
// gcAllocBits live until all the spans have been swept during this GC cycle.
//
// The span's sweep extinguishes all the references to gcBitsArenas.previous
// by pointing gcAllocBits into the gcBitsArenas.current.
//
// The gcBitsArenas.previous is released to the gcBitsArenas.free list.
//
//
// nextMarkBitArenaEpoch()，此函数只被 finishsweep_m() 函数调用。
// gcBitsArenas让msapn的标记位图和分配位图可以被回收复用转换。存在current/previous/next长位图主要是为了保证标记位图和分配位图
// 的生命周期(在还被引用时不能被修改)以及复用效率(用更少的内存做更多的事)。
//
// 此函数对 gcBitsArenas 的current/previous/next字段进行角色转换。为接下来 msapn.allocbits和msapn.gcmarkbits
// 可以安全读写，并在sweept时保证为msapn.gicmarkbits分配bitsmap全部位都是零。且满足不同大小的bitsmap申请需求。
//
// 所有mspan共有这个gcBitsArenas，比如next字段是一个包含N个位的值，每个mspan都可以从中申请(其实是存储next长位图上一个位的地址，
// 长度与mspan.nelemsize相同)不同大小的bitsmap。
// 因为所有mspan的分配位图和标记位上执行的操作在同一GC阶段都是一样的，比如角色互换。所以gcBitsArenas可以使用
// 长bitmaps。
//
// 此函数调用之后，到下一次此函数调用之前：
// gcBitsArenas.previous 保留可读写，被msapn.allocbits引用。
// gcBitsArenas.current 保留可读写，被mspan.gcmarkbits引用。
// gcBitsArenas.next 用于申请bits使用，其位都是零值。在sweep执行时会被mspan.gcmarkbits的新初始值引用。
//
// 此函数做的任务：
// gcBitsArenas.previous 释放到空闲 gcBitsArenas.free 链表中。因为mspan.allocbits已经在sweep中被重新赋值了。所以可以回收了。
// gcBitsArenas.current 变成 gcBitsArenas.previous，被mspan.allocbits引用。
// gcBitsArenas.current 设置成了 gcBitsArenas.next，被msapn.gcmarkbits引用。
//
// -----------------
// 在sweep执行时，mspan.gitmarkbits(引用gcBitsArenas.current，赋值之后，会从gcBitsArenas.next申请新的bits)
// 会赋值给mspan.allocbits(赋值之前mspan.allocbits引用gcBitsArenas.previous)，
// 所以此函数要保留gcBitsArenas.current和gcBitsArenas.next，可回收gcBitsArenas.previous。
// 并转换角色，因为在一次再调用此函数时gcBitsArenas.previous会被回收。
// -----------------
// 对bits进行读写时，存在的引用关系(不考虑短暂的过度状态[sweep执行完毕后到此函数返回之前])：
// mspan.gcmarkbits肯定引用gcBitsArenas.current。
// mspan.allocbits肯定引用gcBitsArenas.previous。
// !!! 不过第一次执行GC时此函数调用之前它们引用的都是gcBitsArenas.next，调用之后引用的都是
// gcBitsArenas.current直到再次调用此函数之后(因为刚刚经历了第一次sweep阶段)就满足了上面的情况了。
// -----------------
func nextMarkBitArenaEpoch() {
	lock(&gcBitsArenas.lock)
	// 第一轮GC时， gcBitsArenas.previous 为nil。
	if gcBitsArenas.previous != nil {
		// 回收 gcBitsArenas.previous
		if gcBitsArenas.free == nil {
			gcBitsArenas.free = gcBitsArenas.previous
		} else {
			// 将 gcBitsArenas.free 链表接到 gcBitsArenas.previous 后面，并
			// 将 gcBitsArenas.free 设置为 gcBitsArenas.previous，这样便回收了 gcBitsArenas.previous。
			// Find end of previous arenas.
			last := gcBitsArenas.previous
			for last = gcBitsArenas.previous; last.next != nil; last = last.next {
			}
			last.next = gcBitsArenas.free
			gcBitsArenas.free = gcBitsArenas.previous
		}
	}
	// gcBitsArenas.next 是上一轮 sweep 为下一轮GC准备的。
	gcBitsArenas.previous = gcBitsArenas.current
	gcBitsArenas.current = gcBitsArenas.next
	// 将gcBitsArenas.next设置为nil，这样在从其申请bits时，其会从free链表取出一个节点并将其全部位置0。
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), nil) // newMarkBits calls newArena when needed
	unlock(&gcBitsArenas.lock)
}

// newArenaMayUnlock allocates and zeroes a gcBits arena.
// The caller must hold gcBitsArena.lock. This may temporarily release it.
func newArenaMayUnlock() *gcBitsArena {
	var result *gcBitsArena
	if gcBitsArenas.free == nil {
		unlock(&gcBitsArenas.lock)
		result = (*gcBitsArena)(sysAlloc(gcBitsChunkBytes, &memstats.gcMiscSys))
		if result == nil {
			throw("runtime: cannot allocate memory")
		}
		lock(&gcBitsArenas.lock)
	} else {
		result = gcBitsArenas.free
		gcBitsArenas.free = gcBitsArenas.free.next
		memclrNoHeapPointers(unsafe.Pointer(result), gcBitsChunkBytes)
	}
	result.next = nil
	// If result.bits is not 8 byte aligned adjust index so
	// that &result.bits[result.free] is 8 byte aligned.
	if uintptr(unsafe.Offsetof(gcBitsArena{}.bits))&7 == 0 {
		result.free = 0
	} else {
		result.free = 8 - (uintptr(unsafe.Pointer(&result.bits[0])) & 7)
	}
	return result
}
