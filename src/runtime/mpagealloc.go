// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Page allocator.
//
// The page allocator manages mapped pages (defined by pageSize, NOT
// physPageSize) for allocation and re-use. It is embedded into mheap.
//
// Pages are managed using a bitmap that is sharded into chunks.
// In the bitmap, 1 means in-use, and 0 means free. The bitmap spans the
// process's address space.
//
// Chunks are managed in a sparse-array-style structure
// similar to mheap.arenas, since the bitmap may be large on some systems.
//
// The bitmap is efficiently searched by using a radix tree in combination
// with fast bit-wise intrinsics. Allocation is performed using an address-ordered
// first-fit approach.
//
// Each entry in the radix tree is a summary that describes three properties of
// a particular region of the address space: the number of contiguous free pages
// at the start and end of the region it represents, and the maximum number of
// contiguous free pages found anywhere in that region.
//
// Each level of the radix tree is stored as one contiguous array, which represents
// a different granularity of subdivision of the processes' address space. Thus, this
// radix tree is actually implicit in these large arrays, as opposed to having explicit
// dynamically-allocated pointer-based node structures. Naturally, these arrays may be
// quite large for system with large address spaces, so in these cases they are mapped
// into memory as needed. The leaf summaries of the tree correspond to a bitmap chunk.
//
// The root level (referred to as L0 and index 0 in pageAlloc.summary) has each
// summary represent the largest section of address space (16 GiB on 64-bit systems),
// with each subsequent level representing successively smaller subsections until we
// reach the finest granularity at the leaves, a chunk.
//
// **More specifically, each summary in each level (except for leaf summaries)
// represents some number of entries in the following level.
// For example, each
// summary in the root level may represent a 16 GiB region of address space,
// and in the next level there could be 8 corresponding entries which represent 2
// GiB subsections of that 16 GiB region, each of which could correspond to 8
// entries in the next level which each represent 256 MiB regions, and so on.**
//
// Thus, this design only scales to heaps so large, but can always be extended to
// larger heaps by simply adding levels to the radix tree, which mostly costs
// additional virtual address space.
// The choice of managing large arrays also means
// that a large amount of virtual address space may be reserved by the runtime.

package runtime

import (
	"unsafe"
)
// 16GB -> 2GB -> 256MB -> 32MB ->4MB(leaf)
const (
	// The size of a bitmap chunk, i.e. the amount of bits (that is, pages) to consider
	// in the bitmap at once.
	//
	// 一个chunk包含多少页
	// 2^9 512页
	pallocChunkPages    = 1 << logPallocChunkPages
	// 一个chunk表示的页大大小为：4M
	pallocChunkBytes    = pallocChunkPages * pageSize
	//
	logPallocChunkPages = 9
	//
	logPallocChunkBytes = logPallocChunkPages + pageShift

	// The number of radix bits for each level.
	//
	// The value of 3 is chosen such that the block of summaries we need to scan at
	// each level fits in 64 bytes (2^3 summaries * 8 bytes per summary), which is
	// close to the L1 cache line width on many systems.
	// Also, a value of 3 fits 4 tree
	// levels perfectly into the 21-bit pallocBits summary field at the root level.
	//
	// 16GB = 2^34  21bit+13bit 2^34字节。
	//
	// The following equation explains how each of the constants relate:
	// 14 + (5-1)*3 + 22  = 48
	// summaryL0Bits + (summaryLevels-1)*summaryLevelBits + logPallocChunkBytes = heapAddrBits
	//
	// summaryLevels is an architecture-dependent value defined in mpagealloc_*.go.
	summaryLevelBits = 3
	// summaryL0Bits = 14
	summaryL0Bits  = heapAddrBits - logPallocChunkBytes - (summaryLevels-1)*summaryLevelBits

	// pallocChunksL2Bits is the number of bits of the chunk index number
	// covered by the second level of the chunks map.
	//
	// See (*pageAlloc).chunks for more details. Update the documentation
	// there should this change.
	// 48 - 22 - 13 = 13
	pallocChunksL2Bits  = heapAddrBits - logPallocChunkBytes - pallocChunksL1Bits
	// 13
	pallocChunksL1Shift = pallocChunksL2Bits
)

// maxSearchAddr returns the maximum searchAddr value, which indicates
// that the heap has no free space.
//
// This function exists just to make it clear that this is the maximum address
// for the page allocator's search space. See maxOffAddr for details.
//
// It's a function (rather than a variable) because it needs to be
// usable before package runtime's dynamic initialization is complete.
// See #51913 for details.
//
// 0x00007FFFFFFFFFFF
func maxSearchAddr() offAddr { return maxOffAddr }

// Global chunk index.
//
// Represents an index into the leaf level of the radix tree.
// Similar to arenaIndex, except instead of arenas, it divides the address
// space into chunks.
//
// 分解成两级然后，用于索引 pageAlloc.chunks 中的值。
type chunkIdx uint

// chunkIndex returns the global index of the palloc chunk containing the
// pointer p.
//
// 根据地址p，返回其所属的chunk的编号从0开始。
func chunkIndex(p uintptr) chunkIdx {
	// pallocChunkBytes 0x400000 2^22 4MB
	// arenaBaseOffset=0xFFFF800000000000
	// 返回地址p所在的chunk在所有chunk中的偏移量。
	//
	// p-arenaBaseOffset是个负值/4M(一个chunk可以表示的地址空间)
	// chunkIdx会将此负值转换为正数，而p的最大值为0x00007FFFFFFFFFFF-1。
	// unint(maxOffAddr) - arenaBaseOffset = 281474976710655 = 0x0000FFFFFFFFFFFF
	return chunkIdx((p - arenaBaseOffset) / pallocChunkBytes)
}

// chunkBase returns the base address of the palloc chunk at index ci.
// 这个chunk的起始地址。真实虚拟地址 var a = 100 println(&a)
func chunkBase(ci chunkIdx) uintptr {
	return uintptr(ci)*pallocChunkBytes + arenaBaseOffset
}

// chunkPageIndex computes the index of the page that contains p,
// relative to the chunk which contains p.
//
// 在这个chunk中，p所在的页的偏移量，相对于这个chunk的起始页而言。
func chunkPageIndex(p uintptr) uint {
	return uint(p % pallocChunkBytes / pageSize)
}

// l1 returns the index into the first level of (*pageAlloc).chunks.
func (i chunkIdx) l1() uint {
	if pallocChunksL1Bits == 0 {
		// Let the compiler optimize this away if there's no
		// L1 map.
		return 0
	} else {
		return uint(i) >> pallocChunksL1Shift
	}
}

// l2 returns the index into the second level of (*pageAlloc).chunks.
func (i chunkIdx) l2() uint {
	if pallocChunksL1Bits == 0 {
		return uint(i)
	} else {
		return uint(i) & (1<<pallocChunksL2Bits - 1)
	}
}

// offAddrToLevelIndex converts an address in the offset address space
// to the index into summary[level] containing addr.
func offAddrToLevelIndex(level int, addr offAddr) int {
	return int((addr.a - arenaBaseOffset) >> levelShift[level])
}

// levelIndexToOffAddr converts an index into summary[level] into
// the corresponding address in the offset address space.
func levelIndexToOffAddr(level, idx int) offAddr {
	return offAddr{(uintptr(idx) << levelShift[level]) + arenaBaseOffset}
}

// addrsToSummaryRange converts base and limit pointers into a range
// of entries for the given summary level.
//
// The returned range is inclusive on the lower bound and exclusive on
// the upper bound.
func addrsToSummaryRange(level int, base, limit uintptr) (lo int, hi int) {
	// This is slightly more nuanced than just a shift for the exclusive
	// upper-bound. Note that the exclusive upper bound may be within a
	// summary at this level, meaning if we just do the obvious computation
	// hi will end up being an inclusive upper bound. Unfortunately, just
	// adding 1 to that is too broad since we might be on the very edge
	// of a summary's max page count boundary for this level
	// (1 << levelLogPages[level]). So, make limit an inclusive upper bound
	// then shift, then add 1, so we get an exclusive upper bound at the end.
	//
	// [base,limit)此地址区间在level级别的元数据分布在从lo entry到hi entry中，
	// 不包括hi entry。
	lo = int((base - arenaBaseOffset) >> levelShift[level])
	hi = int(((limit-1)-arenaBaseOffset)>>levelShift[level]) + 1
	return
}

// blockAlignSummaryRange aligns indices into the given level to that
// level's block width (1 << levelBits[level]). It assumes lo is inclusive
// and hi is exclusive, and so aligns them down and up respectively.
func blockAlignSummaryRange(level int, lo, hi int) (int, int) {
	/**
	level 8240 8241
	level 65920 65921
	level 527360 527361
	level 4218880 4218881
	level 33751040 33751041
	*/
	e := uintptr(1) << levelBits[level]
	// println("after align",int(alignDown(uintptr(lo), e)), int(alignUp(uintptr(hi), e)))
	/**
	after align 0 16384
	after align 65920 65928
	after align 527360 527368
	after align 4218880 4218888
	after align 33751040 33751048
	*/
	return int(alignDown(uintptr(lo), e)), int(alignUp(uintptr(hi), e))
}

type pageAlloc struct {
	// Radix tree of summaries.
	//
	// Each slice's cap represents the whole memory reservation.
	// Each slice's len reflects the allocator's maximum known
	// mapped heap address for that level.
	//
	// The backing store of each summary level is reserved in init
	// and may or may not be committed in grow (small address spaces
	// may commit all the memory in init).
	//
	// The purpose of keeping len <= cap is to enforce bounds checks
	// on the top end of the slice so that instead of an unknown
	// runtime segmentation fault, we get a much friendlier out-of-bounds
	// error.
	//
	// To iterate over a summary level, use inUse to determine which ranges
	// are currently available. Otherwise one might try to access
	// memory which is only Reserved which may result in a hard fault.
	//
	// We may still get segmentation faults < len since some of that
	// memory may not be committed yet.
	//
	// b(ytes) = 2^14*8,2^17*8,2^20*8,2^23*8,2^26*8
	// cap([]pollocSum) = 2^14,2^17,2^20,2^23,2^26
	// b表示每个层级用于存储元数据所需的字节数。
	// 比如最后一层，每个entry可以表4MB但amd64平台下共有2^48字节的地址空间，所以需要2^48/4M=2^26个entry，
	// 每个entry需要一个pallocSum类型的值用于保存页使用情况的元数据。
	// 所以最后一层需要2^26个pallocSum来保存这一层级所有entry的元数据。
	//
	// cap(sumary[0])=2^14 //用第一层级中的entry来表示整个地址空间页使用情况，需要2^14个pallocSum。
	// cap(sumary[1])=2^17 //用第二层级中的entry来表示整个地址空间页使用情况，需要2^17个pallocSum。
	// cap(sumary[1])=2^20 //用第三层级中的entry来表示整个地址空间页使用情况，空间需要2^20个pallocSum。
	// cap(sumary[1])=2^23 //用第四层级中的entry来表示整个地址空间页使用情况，空间需要2^23个pallocSum。
	// cap(sumary[1])=2^26 //用第五层级中的entry来表示整个地址空间页使用情况，空间需要2^26个pallocSum。
	// 这些元数据所需的地址空间已经在 pageAlloc.init 中通过调用 sysReserve 函数进行了保留。
	// summaryLeves=5
	// pageAlloc.update 函数中更新，每当页的使用情况改变(新增、分配、释放)对应的512位图发生变更
	// 就会更新页所表示的地址区间在sum各层级对应的entry中统计信息。
	// 改变的页数会向上对齐到一个chunk(512个页)。因为这是最后一层entry对应的单位。
	summary [summaryLevels][]pallocSum

	// chunks is a slice of bitmap chunks.
	//
	// The total size of chunks is quite large on most 64-bit platforms
	// (O(GiB) or more) if flattened, so rather than making one large mapping
	// (which has problems on some platforms, even when PROT_NONE) we use a
	// two-level sparse array approach similar to the arena index in mheap.
	//
	// To find the chunk containing a memory address `a`, do:
	//   chunkOf(chunkIndex(a))
	//
	// Below is a table describing the configuration for chunks for various
	// heapAddrBits supported by the runtime.
	// 一个chunk可以表示4M(2^22)地址空间，48位平台需要2^26次方个chunk。
	// heapAddrBits | L1 Bits | L2 Bits | L2 Entry Size
	// ------------------------------------------------
	// 32           | 0       | 10      | 128 KiB
	// 33 (iOS)     | 0       | 11      | 256 KiB
	// 48           | 13      | 13      | 1 MiB
	//
	// There's no reason to use the L1 part of chunks on 32-bit, the
	// address space is small so the L2 is small. For platforms with a
	// 48-bit address space, we pick the L1 such that the L2 is 1 MiB
	// in size, which is a good balance between low granularity without
	// making the impact on BSS too high (note the L1 is stored directly
	// in pageAlloc).
	//
	// To iterate over the bitmap, use inUse to determine which ranges
	// are currently available. Otherwise one might iterate over unused
	// ranges.
	//
	// Protected by mheapLock.
	//
	// TODO(mknyszek): Consider changing the definition of the bitmap
	// such that 1 means free and 0 means in-use so that summaries and
	// the bitmaps align better on zero-values.
	//
	// chunks[13][13]pallocData
	// pallocData 的大小是128字节,1024个位，其中前512表示一个chunk分配情况，后512位表示scanveged情况。
	// 一个chunk可以表示4MB的地址空间，需要2^48-2^22=2^26个chunk。
	// l2的大小为2^13*2^7(128kb，2个512位)=1MB。
	// 用两级悉数数组来索引2^26个chunk。
	//
	// 所有的chunk从低地址到高地址排序，索引0的chunk对应的地址空间中的地址值比索引1的低。
	chunks [1 << pallocChunksL1Bits]*[1 << pallocChunksL2Bits]pallocData

	// The address to start an allocation search with. It must never
	// point to any memory that is not contained in inUse, i.e.
	// inUse.contains(searchAddr.addr()) must always be true. The one
	// exception to this rule is that it may take on the value of
	// maxOffAddr to indicate that the heap is exhausted.
	//
	// We guarantee that all valid heap addresses below this value
	// are allocated and not worth searching.
	//
	// 初始值 maxSearchAddr()=0x00007FFFFFFFFFFF
	// 页分配指示器，比这个地址小的地址都已经被分配了，不必再搜索了。
	// 从这个地址开始搜索。
	//
	// p.searchAddr.addr()如果大于end，表示所有的inUse都已经用完了。
	//
	// searchAddr肯定是一个整页的其实地址。
	// 
	// 如果需分配的npages恰巧是从searchAddr开始分配的，就会出现需要更新的情况。
	// 00111000 分配位图部分，若searchIdx对应的是第一个0，如果需要3页。那么p.searchAddr
	// 便不需要更新，如果需要2页变化更新。相应的3个0连续位或者两个0连续位都会会被置为1。
	//
	// searchAddr is not allowed to point into unmapped heap memory
	// unless it is maxSearchAddr
	searchAddr offAddr

	// start and end represent the chunk indices
	// which pageAlloc knows about. It assumes
	// chunks in the range [start, end) are
	// currently ready to use.
	//
	// 对应 [pageAlloc.chunks[start], pageAlloc.chunks[end])。
	// 不包括 pageAlloc.chunks[end] 这个chunk。
	// start 是 inUse 中第一个addrRange.limit所在的chunkIdx。
	// end 是 inUse 中最后一个addrRange.limit所在的chunkIdx。
	//
	// p.searchAddr.addr()如果大于end，表示所有的inUse都已经用完了。
	start, end chunkIdx

	// inUse is a slice of ranges of address space which are
	// known by the page allocator to be currently in-use (passed
	// to grow).
	//
	// This field is currently unused on 32-bit architectures but
	// is harmless to track. We care much more about having a
	// contiguous heap in these cases and take additional measures
	// to ensure that, so in nearly all cases this should have just
	// 1 element.
	//
	// All access is protected by the mheapLock.
	//
	// 已经分配的地址区间(可供使用)的地址信息。
	// start end 记录了这些地址区间跨越的chunks的首尾索引(并没有包含end对应的chunk)
	inUse addrRanges

	// scav stores the scavenger state.
	scav struct {
		// index is an efficient index of chunks that have pages available to
		// scavenge.
		//
		// 用于维护chunk的清扫状态。
		index scavengeIndex

		// released is the amount of memory released this scavenge cycle.
		//
		// Updated atomically.
		//
		// 如果没有开启 debug.scavtrace，则此值会一直累加。
		// 在bgscavenge函数中累加。所以只是统计了后台scavenge工作者回收的字节数。
		//
		// 开启debug.scavtrace，则会在每次打印scavenge统计信息之后清空。
		released uintptr
	}

	// mheap_.lock. This level of indirection makes it possible
	// to test pageAlloc indepedently of the runtime allocator.
	mheapLock *mutex

	// sysStat is the runtime memstat to update when new system
	// memory is committed by the pageAlloc for allocation metadata.
	//
	// 类型 memstats.gcMiscSys
	// chunks
	// 统计页分配时，一些记录这些页的元数据结构所占的内存。
	sysStat *sysMemStat

	// summaryMappedReady is the number of bytes mapped in the Ready state
	// in the summary structure. Used only for testing currently.
	//
	// Protected by mheapLock.
	summaryMappedReady uintptr

	// Whether or not this struct is being used in tests.
	test bool
}

func (p *pageAlloc) init(mheapLock *mutex, sysStat *sysMemStat) {
	// 21>21 false
	if levelLogPages[0] > logMaxPackedValue {
		// We can't represent 1<<levelLogPages[0] pages, the maximum number
		// of pages we need to represent at the root level, in a summary, which
		// is a big problem. Throw.
		print("runtime: root level max pages = ", 1<<levelLogPages[0], "\n")
		print("runtime: summary max pages = ", maxPackedValue, "\n")
		throw("root level max pages doesn't fit in summary")
	}
	// memstats.gcMiscSys
	p.sysStat = sysStat

	// Initialize p.inUse.
	p.inUse.init(sysStat)

	// System-dependent initialization.
	p.sysInit()

	// Start with the searchAddr in a state indicating there's no free memory.
	p.searchAddr = maxSearchAddr()

	// Set the mheapLock.
	p.mheapLock = mheapLock
}

// tryChunkOf returns the bitmap data for the given chunk.
//
// Returns nil if the chunk data has not been mapped.
func (p *pageAlloc) tryChunkOf(ci chunkIdx) *pallocData {
	l2 := p.chunks[ci.l1()]
	if l2 == nil {
		return nil
	}
	return &l2[ci.l2()]
}

// chunkOf returns the chunk at the given chunk index.
//
// The chunk index must be valid or this method may throw.
func (p *pageAlloc) chunkOf(ci chunkIdx) *pallocData {
	return &p.chunks[ci.l1()][ci.l2()]
}

// grow sets up the metadata for the address range [base, base+size).
// It may allocate metadata, in which case *p.sysStat will be updated.
//
// 目前只被 mheap.grow 方法所调用。
// 在此函数调用之前：[base,base+size)这段地址空间已经被 sysMap (prepared状态)。
//
// 这里执行的任务是:
// 1. 为这段地址空间对应的元数据信息的结构分配内存。
//    1.1
// 2. 更新对应的元数据信息。
//    2.1 pageAlloc.chunks 中的512分配位图置0和512scav位图置1。
//    2.2 pageAlloc.summary 中的此区间在各层对应的sum信息。
//    2.3 searchAddr 按条件更新此字段。
//    2.4 start end 按条件更新这两个字段。
//    2.5 inUse base，size表示的地址区间添加到p.inUse中的适当位置处。
//
// size肯定是4M的倍数。
// p.mheapLock must be held.
func (p *pageAlloc) grow(base, size uintptr) {
	assertLockHeld(p.mheapLock)

	// Round up to chunks, since we can't deal with increments smaller
	// than chunks. Also, sysGrow expects aligned values.
	limit := alignUp(base+size, pallocChunkBytes)
	base = alignDown(base, pallocChunkBytes)

	// Grow the summary levels in a system-dependent manner.
	// We just update a bunch of additional metadata here.
	//
	// 根据base，limit计算出它们表示的地址区间在pageAlloc.summary和
	// pageAlloc.scav.index.chunks对应的元素(表示这段地址区间的元数据信息)。
	// 将pageAlloc.summary中对应的元素和pageAlloc.scav.index.chunks中这些对应的元素
	// 的地址空间变成reay状态(mallocgc初始化时这些区间只是被reversed)。
	//
	// p.sysGrow只负责存储元数据信息结构所占内存的分配，并不会更新这些元数据信息。
	p.sysGrow(base, limit)

	// Update p.start and p.end.
	// If no growth happened yet, start == 0. This is generally
	// safe since the zero page is unmapped.
	//
	firstGrowth := p.start == 0
	start, end := chunkIndex(base), chunkIndex(limit)
	// 第一次提调用adm64
	// println(start,end)
	// 33751040 33751041
	if firstGrowth || start < p.start {
		p.start = start
	}
	if end > p.end {
		p.end = end
	}
	// Note that [base, limit) will never overlap with any existing
	// range inUse because grow only ever adds never-used memory
	// regions to the page allocator.
	//
	// 将base，limit表示的addrRange插入到p.inUse中适当位置。
	p.inUse.add(makeAddrRange(base, limit))
	// 第一次调用输出：
	// println("--",p.inUse.ranges[0].base.a,p.inUse.ranges[0].limit.a)
	// println(p.inUse.totalBytes)
	// -- 824633720832 824637915136
	// 4194304=4MB

	// A grow operation is a lot like a free operation, so if our
	// chunk ends up below p.searchAddr, update p.searchAddr to the
	// new address, just like in free.
	//
	// 新分配的地址区间是可供搜索的，因为还没有被使用。
	if b := (offAddr{base}); b.lessThan(p.searchAddr) {
		// 第一次调用：
		// println("p.searchAddr.a",p.searchAddr.a)
		// p.searchAddr.a 824633720832=0xC000000000
		p.searchAddr = b
	}

	// Add entries into chunks, which is sparse, if needed. Then,
	// initialize the bitmap.
	//
	// Newly-grown memory is always considered scavenged.
	// Set all the bits in the scavenged bitmaps high.
	for c := chunkIndex(base); c < chunkIndex(limit); c++ {
		// [base,limit)可能跨越多个chunk，但肯定是整数个。
		// println("---",len(p.chunks))
		// --- 8192
		if p.chunks[c.l1()] == nil {
			// Create the necessary l2 entry.
			// 返回p.chunks[0]的值，其指向的是一个类型为[2^13]pallocData的数组。
			r := sysAlloc(unsafe.Sizeof(*p.chunks[0]), p.sysStat)
			if r == nil {
				throw("pageAlloc: out of memory")
			}
			// Store the new chunk block but avoid a write barrier.
			// grow is used in call chains that disallow write barriers.
			*(*uintptr)(unsafe.Pointer(&p.chunks[c.l1()])) = uintptr(r)
		}
		// 第一次调用(0,512)
		// 全部位设置成1。
		p.chunkOf(c).scavenged.setRange(0, pallocChunkPages)
		// 第一次调用：
		// println("***",p.chunkOf(c).scavenged[0])
		// println("***",p.chunkOf(c).scavenged[1])
		// println("***",p.chunkOf(c).scavenged[7])
		// *** 18446744073709551615
		// *** 18446744073709551615
		// *** 18446744073709551615
	}

	// Update summaries accordingly. The grow acts like a free, so
	// we need to ensure this newly-free memory is visible in the
	// summaries.
	//
	// 第一次调用时为(0xC000000000,4MB/8192=512,true,false)
	p.update(base, size/pageSize, true, false)
}

// update updates heap metadata. It must be called each time the bitmap(chunk对应的512位位图)
// is updated.
//
// If contig is true, update does some optimizations assuming that there was
// a contiguous allocation or free between addr and addr+npages. alloc indicates
// whether the operation performed was an allocation or a free.
//
// p.mheapLock must be held.
//
// 此函数在分配位图发生变更时，同步 pageAlloc.summary 各层级的sum信息。
//
// 因为为mspan分配内存时使用的整页(8192)，但调用此函数npages却要是整数倍的chunk对应的页数(512页)。
// 比如新grow了chunk(512页,4MB)，然后分配了512个msapn(每个占用一页)则这个函数就会被调用512次。
// 因为一旦一个chunk对应的512位图发生了变化就需调用此函数更新每一层级对应的sum(一个entry对应一个sum)信息。
//
// 参数：
// npages 第一页的起始地址
// npages 从base开始总共包含了多少页
// contig 等于true，表示npages都已经被分配了或者都已经被释放了。
// alloc 表是释放还是分配，即页已经被使用(比如用于mspan)true，还是接下来可以使用false。新分配的页alloc是false。
//
// [base,base+npages*8192)
func (p *pageAlloc) update(base, npages uintptr, contig, alloc bool) {
	assertLockHeld(p.mheapLock)

	// base, limit, start, and end are inclusive.
	limit := base + npages*pageSize - 1
	sc, ec := chunkIndex(base), chunkIndex(limit)
	// 第一次调用
	// println("sc,ec",sc,ec)
	// println("npages",npages)
	// 33751040 33751040

	// Handle updating the lowest level first.
	if sc == ec {
		// Fast path: the allocation doesn't span more than one chunk,
		// so update this one and if the summary didn't change, return.
		x := p.summary[len(p.summary)-1][sc]
		// 第一次调用
		// println("x",x)
		// x 0
		y := p.chunkOf(sc).summarize()
		// y=packPallocSum(512,512,512)
		if x == y {
			return
		}
		// 更新最后一层对应的(entry)sum的信息。
		p.summary[len(p.summary)-1][sc] = y
	} else if contig {
		// Slow contiguous path: the allocation spans more than one chunk
		// and at least one summary is guaranteed to change.
		summary := p.summary[len(p.summary)-1]

		// Update the summary for chunk sc.
		summary[sc] = p.chunkOf(sc).summarize()

		// Update the summaries for chunks in between, which are
		// either totally allocated or freed.
		whole := p.summary[len(p.summary)-1][sc+1 : ec]
		if alloc {
			// Should optimize into a memclr.
			for i := range whole {
				whole[i] = 0
			}
		} else {
			for i := range whole {
				whole[i] = freeChunkSum
			}
		}

		// Update the summary for chunk ec.
		summary[ec] = p.chunkOf(ec).summarize()
	} else {
		// Slow general path: the allocation spans more than one chunk
		// and at least one summary is guaranteed to change.
		//
		// We can't assume a contiguous allocation happened, so walk over
		// every chunk in the range and manually recompute the summary.
		summary := p.summary[len(p.summary)-1]
		for c := sc; c <= ec; c++ {
			summary[c] = p.chunkOf(c).summarize()
		}
	}

	// Walk up the radix tree and update the summaries appropriately.
	changed := true
	for l := len(p.summary) - 2; l >= 0 && changed; l-- {
		// Update summaries at level l from summaries at level l+1.
		changed = false

		// "Constants" for the previous level which we
		// need to compute the summary from that level.
		logEntriesPerBlock := levelBits[l+1]
		logMaxPages := levelLogPages[l+1]

		// lo and hi describe all the parts of the level we need to look at.
		lo, hi := addrsToSummaryRange(l, base, limit+1)

		// Iterate over each block, updating the corresponding summary in the less-granular level.
		for i := lo; i < hi; i++ {
			children := p.summary[l+1][i<<logEntriesPerBlock : (i+1)<<logEntriesPerBlock]
			sum := mergeSummaries(children, logMaxPages)
			old := p.summary[l][i]
			if old != sum {
				changed = true
				p.summary[l][i] = sum
			}
		}
	}
}

// allocRange marks the range of memory [base, base+npages*pageSize) as
// allocated. It also updates the summaries to reflect the newly-updated
// bitmap.
//
// Returns the amount of scavenged memory in bytes present in the
// allocated range.
//
// p.mheapLock must be held.
// 从 pageAlloc 的inUse中分配[base,base+napges*pageSize)地址区间，其实只是更新对应的
// alloc bitmap/scavenged bitmap 和 pageAlloc.summary 中的sum信息。即做些记薄工作。
//
// 参数: npages要分配的页数。
// 返回值：这些页对应的 pageAlloc.chunks 中的 pallocData.scavenged 位中为1的位的个数。
//
// 影响: 更新 pageAlloc.chunks 中[base,base+npages*pageSize)影响到的chunk中的512分配位(由0变为1)和
// 512 scavenged位(由1变为0)。并更新 pageAlloc.summary 中各个层级对应的sum信息。
func (p *pageAlloc) allocRange(base, npages uintptr) uintptr {
	assertLockHeld(p.mheapLock)

	limit := base + npages*pageSize - 1
	sc, ec := chunkIndex(base), chunkIndex(limit)
	si, ei := chunkPageIndex(base), chunkPageIndex(limit)

	scav := uint(0)
	if sc == ec {
		// The range doesn't cross any chunk boundaries.
		chunk := p.chunkOf(sc)
		scav += chunk.scavenged.popcntRange(si, ei+1-si)
		chunk.allocRange(si, ei+1-si)
	} else {
		// The range crosses at least one chunk boundary.
		chunk := p.chunkOf(sc)
		scav += chunk.scavenged.popcntRange(si, pallocChunkPages-si)
		chunk.allocRange(si, pallocChunkPages-si)
		for c := sc + 1; c < ec; c++ {
			chunk := p.chunkOf(c)
			scav += chunk.scavenged.popcntRange(0, pallocChunkPages)
			chunk.allocAll()
		}
		chunk = p.chunkOf(ec)
		scav += chunk.scavenged.popcntRange(0, ei+1)
		chunk.allocRange(0, ei+1)
	}
	p.update(base, npages, true, true)
	return uintptr(scav) * pageSize
}

// findMappedAddr returns the smallest mapped offAddr that is
// >= addr. That is, if addr refers to mapped memory, then it is
// returned. If addr is higher than any mapped region, then
// it returns maxOffAddr.
//
// p.mheapLock must be held.
func (p *pageAlloc) findMappedAddr(addr offAddr) offAddr {
	assertLockHeld(p.mheapLock)

	// If we're not in a test, validate first by checking mheap_.arenas.
	// This is a fast path which is only safe to use outside of testing.
	ai := arenaIndex(addr.addr())
	if p.test || mheap_.arenas[ai.l1()] == nil || mheap_.arenas[ai.l1()][ai.l2()] == nil {
		vAddr, ok := p.inUse.findAddrGreaterEqual(addr.addr())
		if ok {
			return offAddr{vAddr}
		} else {
			// The candidate search address is greater than any
			// known address, which means we definitely have no
			// free memory left.
			return maxOffAddr
		}
	}
	return addr
}

// find searches for the first (address-ordered) contiguous free region of
// npages in size and returns a base address for that region.
//
// It uses p.searchAddr to prune its search and assumes that no palloc chunks
// below chunkIndex(p.searchAddr) contain any free memory at all.
//
// find also computes and returns a candidate p.searchAddr, which may or
// may not prune more of the address space than p.searchAddr already does.
// This candidate is always a valid p.searchAddr.
//
// find represents the slow path and the full radix tree search.
//
// Returns a base address of 0 on failure, in which case the candidate
// searchAddr returned is invalid and must be ignored.
//
// p.mheapLock must be held.
func (p *pageAlloc) find(npages uintptr) (uintptr, offAddr) {
	assertLockHeld(p.mheapLock)

	// Search algorithm.
	//
	// This algorithm walks each level l of the radix tree from the root level
	// to the leaf level. It iterates over at most 1 << levelBits[l] of entries
	// in a given level in the radix tree, and uses the summary information to
	// find either:
	//  1) That a given subtree contains a large enough contiguous region, at
	//     which point it continues iterating on the next level, or
	//  2) That there are enough contiguous boundary-crossing bits to satisfy
	//     the allocation, at which point it knows exactly where to start
	//     allocating from.
	//
	// i tracks the index into the current level l's structure for the
	// contiguous 1 << levelBits[l] entries we're actually interested in.
	//
	// NOTE: Technically this search could allocate a region which crosses
	// the arenaBaseOffset boundary, which when arenaBaseOffset != 0, is
	// a discontinuity. However, the only way this could happen is if the
	// page at the zero address is mapped, and this is impossible on
	// every system we support where arenaBaseOffset != 0. So, the
	// discontinuity is already encoded in the fact that the OS will never
	// map the zero page for us, and this function doesn't try to handle
	// this case in any way.

	// i is the beginning of the block of entries we're searching at the
	// current level.
	i := 0

	// firstFree is the region of address space that we are certain to
	// find the first free page in the heap. base and bound are the inclusive
	// bounds of this window, and both are addresses in the linearized, contiguous
	// view of the address space (with arenaBaseOffset pre-added). At each level,
	// this window is narrowed as we find the memory region containing the
	// first free page of memory. To begin with, the range reflects the
	// full process address space.
	//
	// firstFree is updated by calling foundFree each time free space in the
	// heap is discovered.
	//
	// At the end of the search, base.addr() is the best new
	// searchAddr we could deduce in this search.
	firstFree := struct {
		base, bound offAddr
	}{
		base:  minOffAddr,
		bound: maxOffAddr,
	}
	// foundFree takes the given address range [addr, addr+size) and
	// updates firstFree if it is a narrower range. The input range must
	// either be fully contained within firstFree or not overlap with it
	// at all.
	//
	// This way, we'll record the first summary we find with any free
	// pages on the root level and narrow that down if we descend into
	// that summary. But as soon as we need to iterate beyond that summary
	// in a level to find a large enough range, we'll stop narrowing.
	foundFree := func(addr offAddr, size uintptr) {
		if firstFree.base.lessEqual(addr) && addr.add(size-1).lessEqual(firstFree.bound) {
			// This range fits within the current firstFree window, so narrow
			// down the firstFree window to the base and bound of this range.
			firstFree.base = addr
			firstFree.bound = addr.add(size - 1)
		} else if !(addr.add(size-1).lessThan(firstFree.base) || firstFree.bound.lessThan(addr)) {
			// This range only partially overlaps with the firstFree range,
			// so throw.
			print("runtime: addr = ", hex(addr.addr()), ", size = ", size, "\n")
			print("runtime: base = ", hex(firstFree.base.addr()), ", bound = ", hex(firstFree.bound.addr()), "\n")
			throw("range partially overlaps")
		}
	}

	// lastSum is the summary which we saw on the previous level that made us
	// move on to the next level. Used to print additional information in the
	// case of a catastrophic failure.
	// lastSumIdx is that summary's index in the previous level.
	lastSum := packPallocSum(0, 0, 0)
	lastSumIdx := -1

nextLevel:
	for l := 0; l < len(p.summary); l++ {
		// For the root level, entriesPerBlock is the whole level.
		entriesPerBlock := 1 << levelBits[l]
		logMaxPages := levelLogPages[l]

		// We've moved into a new level, so let's update i to our new
		// starting index. This is a no-op for level 0.
		i <<= levelBits[l]

		// Slice out the block of entries we care about.
		entries := p.summary[l][i : i+entriesPerBlock]

		// Determine j0, the first index we should start iterating from.
		// The searchAddr may help us eliminate iterations if we followed the
		// searchAddr on the previous level or we're on the root level, in which
		// case the searchAddr should be the same as i after levelShift.
		j0 := 0
		if searchIdx := offAddrToLevelIndex(l, p.searchAddr); searchIdx&^(entriesPerBlock-1) == i {
			j0 = searchIdx & (entriesPerBlock - 1)
		}

		// Run over the level entries looking for
		// a contiguous run of at least npages either
		// within an entry or across entries.
		//
		// base contains the page index (relative to
		// the first entry's first page) of the currently
		// considered run of consecutive pages.
		//
		// size contains the size of the currently considered
		// run of consecutive pages.
		var base, size uint
		for j := j0; j < len(entries); j++ {
			sum := entries[j]
			if sum == 0 {
				// A full entry means we broke any streak and
				// that we should skip it altogether.
				size = 0
				continue
			}

			// We've encountered a non-zero summary which means
			// free memory, so update firstFree.
			foundFree(levelIndexToOffAddr(l, i+j), (uintptr(1)<<logMaxPages)*pageSize)

			s := sum.start()
			if size+s >= uint(npages) {
				// If size == 0 we don't have a run yet,
				// which means base isn't valid. So, set
				// base to the first page in this block.
				if size == 0 {
					base = uint(j) << logMaxPages
				}
				// We hit npages; we're done!
				size += s
				break
			}
			if sum.max() >= uint(npages) {
				// The entry itself contains npages contiguous
				// free pages, so continue on the next level
				// to find that run.
				i += j
				lastSumIdx = i
				lastSum = sum
				continue nextLevel
			}
			if size == 0 || s < 1<<logMaxPages {
				// We either don't have a current run started, or this entry
				// isn't totally free (meaning we can't continue the current
				// one), so try to begin a new run by setting size and base
				// based on sum.end.
				size = sum.end()
				base = uint(j+1)<<logMaxPages - size
				continue
			}
			// The entry is completely free, so continue the run.
			size += 1 << logMaxPages
		}
		if size >= uint(npages) {
			// We found a sufficiently large run of free pages straddling
			// some boundary, so compute the address and return it.
			addr := levelIndexToOffAddr(l, i).add(uintptr(base) * pageSize).addr()
			return addr, p.findMappedAddr(firstFree.base)
		}
		if l == 0 {
			// We're at level zero, so that means we've exhausted our search.
			return 0, maxSearchAddr()
		}

		// We're not at level zero, and we exhausted the level we were looking in.
		// This means that either our calculations were wrong or the level above
		// lied to us. In either case, dump some useful state and throw.
		print("runtime: summary[", l-1, "][", lastSumIdx, "] = ", lastSum.start(), ", ", lastSum.max(), ", ", lastSum.end(), "\n")
		print("runtime: level = ", l, ", npages = ", npages, ", j0 = ", j0, "\n")
		print("runtime: p.searchAddr = ", hex(p.searchAddr.addr()), ", i = ", i, "\n")
		print("runtime: levelShift[level] = ", levelShift[l], ", levelBits[level] = ", levelBits[l], "\n")
		for j := 0; j < len(entries); j++ {
			sum := entries[j]
			print("runtime: summary[", l, "][", i+j, "] = (", sum.start(), ", ", sum.max(), ", ", sum.end(), ")\n")
		}
		throw("bad summary data")
	}

	// Since we've gotten to this point, that means we haven't found a
	// sufficiently-sized free region straddling some boundary (chunk or larger).
	// This means the last summary we inspected must have had a large enough "max"
	// value, so look inside the chunk to find a suitable run.
	//
	// After iterating over all levels, i must contain a chunk index which
	// is what the final level represents.
	ci := chunkIdx(i)
	j, searchIdx := p.chunkOf(ci).find(npages, 0)
	if j == ^uint(0) {
		// We couldn't find any space in this chunk despite the summaries telling
		// us it should be there. There's likely a bug, so dump some state and throw.
		sum := p.summary[len(p.summary)-1][i]
		print("runtime: summary[", len(p.summary)-1, "][", i, "] = (", sum.start(), ", ", sum.max(), ", ", sum.end(), ")\n")
		print("runtime: npages = ", npages, "\n")
		throw("bad summary data")
	}

	// Compute the address at which the free space starts.
	addr := chunkBase(ci) + uintptr(j)*pageSize

	// Since we actually searched the chunk, we may have
	// found an even narrower free window.
	searchAddr := chunkBase(ci) + uintptr(searchIdx)*pageSize
	foundFree(offAddr{searchAddr}, chunkBase(ci+1)-searchAddr)
	return addr, p.findMappedAddr(firstFree.base)
}

// alloc allocates npages worth of memory from the page heap, returning the base
// address for the allocation and the amount of scavenged memory in bytes
// contained in the region [base address, base address + npages*pageSize).
//
// Returns a 0 base address on failure, in which case other returned values
// should be ignored.
//
// p.mheapLock must be held.
//
// Must run on the system stack because p.mheapLock must be held.
//
// [addr,npages*pageSize)这段地址区间已经通过 mheap.grow 变成了prepared状态。
//
// 从pageAlloc中分配npages个页面，返回这npages的起始地址和这些页面中有多少个页面对应
// 的 pageAlloc.chunks 中的 scavenged 位为了1。
//
// 同时会更新被分配的npages的 pageAlloc.chunks 中的分配/scavenged 位图信息。和
// pageAlloc.summary 中各个层级对应的sum信息。
//
// 如果pageAlloc中(从其summary来判断)没有足够的连续npages则第一个返回值为0，此时第二个返回值应当忽略。
//
//go:systemstack
func (p *pageAlloc) alloc(npages uintptr) (addr uintptr, scav uintptr) {
	assertLockHeld(p.mheapLock)

	// If the searchAddr refers to a region which has a higher address than
	// any known chunk, then we know we're out of memory.
	//
	// p.inUse 中的addrRange已经用完了。
	if chunkIndex(p.searchAddr.addr()) >= p.end {
		return 0, 0
	}

	// If npages has a chance of fitting in the chunk where the searchAddr is,
	// search it directly.
	searchAddr := minOffAddr
	if pallocChunkPages-chunkPageIndex(p.searchAddr.addr()) >= uint(npages) {
		// p.searchAddr 所在的chunk剩余可搜索页大于等于npage。
		//
		// npages is guaranteed to be no greater than pallocChunkPages here.
		i := chunkIndex(p.searchAddr.addr())
		if max := p.summary[len(p.summary)-1][i].max(); max >= uint(npages) {
			// p.searchAddr所在的chunk的最大空闲连续页大于npages。
			j, searchIdx := p.chunkOf(i).find(npages, chunkPageIndex(p.searchAddr.addr()))
			if j == ^uint(0) {
				print("runtime: max = ", max, ", npages = ", npages, "\n")
				print("runtime: searchIdx = ", chunkPageIndex(p.searchAddr.addr()), ", p.searchAddr = ", hex(p.searchAddr.addr()), "\n")
				throw("bad summary data")
			}
			addr = chunkBase(i) + uintptr(j)*pageSize
			searchAddr = offAddr{chunkBase(i) + uintptr(searchIdx)*pageSize}
			goto Found
		}
	}
	// We failed to use a searchAddr for one reason or another, so try
	// the slow path.
	addr, searchAddr = p.find(npages)
	if addr == 0 {
		if npages == 1 {
			// We failed to find a single free page, the smallest unit
			// of allocation. This means we know the heap is completely
			// exhausted. Otherwise, the heap still might have free
			// space in it, just not enough contiguous space to
			// accommodate npages.
			p.searchAddr = maxSearchAddr()
		}
		return 0, 0
	}
Found:
	// Go ahead and actually mark the bits now that we have an address.
	//
	// 真正开始做记簿工作。
	scav = p.allocRange(addr, npages)

	// If we found a higher searchAddr, we know that all the
	// heap memory before that searchAddr in an offset address space is
	// allocated, so bump p.searchAddr up to the new one.
	//
	// 如果需分配的npages恰巧是从searchAddr开始分配的，就会出现需要更新的情况。
	// 00111000 分配位图部分，若searchIdx对应的是第一个0，如果需要3页。那么p.searchAddr
	// 便不需要更新，如果需要2页变化更新。相应的3个0连续位或者两个0连续位都会会被置为1。
	if p.searchAddr.lessThan(searchAddr) {
		p.searchAddr = searchAddr
	}
	return addr, scav
}

// free returns npages worth of memory starting at base back to the page heap.
//
// p.mheapLock must be held.
//
// Must run on the system stack because p.mheapLock must be held.
//
// 参数：
// base 要free的地址区间的起始地址。
// npages 从base开始free npges个页面。
// scavenged 表示此npages对应的scavenged位是否是1。
//
// 影响: npages所有的分配位都置为1，如果scavenged位false，则会
// 将npages所跨越的chunks在 p.scav.index.chunks中对应的位
// 置为1，表示此chunk中包含可scavenged的页面。
//
//go:systemstack
func (p *pageAlloc) free(base, npages uintptr, scavenged bool) {
	assertLockHeld(p.mheapLock)

	// If we're freeing pages below the p.searchAddr, update searchAddr.
	if b := (offAddr{base}); b.lessThan(p.searchAddr) {
		p.searchAddr = b
	}
	limit := base + npages*pageSize - 1
	if !scavenged {
		// 如果这些页对应的scavenged位没有被设置为1，则它们所在的chunks
		// 在 p.scav.index.chunks 中对应的位设置为1，表示这些chuks中
		// 保护可scavenged的页。
		p.scav.index.mark(base, limit+1)
	}
	if npages == 1 {
		// Fast path: we're clearing a single bit, and we know exactly
		// where it is, so mark it directly.
		i := chunkIndex(base)
		p.chunkOf(i).free1(chunkPageIndex(base))
	} else {
		// Slow path: we're clearing more bits so we may need to iterate.
		sc, ec := chunkIndex(base), chunkIndex(limit)
		si, ei := chunkPageIndex(base), chunkPageIndex(limit)

		if sc == ec {
			// The range doesn't cross any chunk boundaries.
			p.chunkOf(sc).free(si, ei+1-si)
		} else {
			// The range crosses at least one chunk boundary.
			p.chunkOf(sc).free(si, pallocChunkPages-si)
			for c := sc + 1; c < ec; c++ {
				p.chunkOf(c).freeAll()
			}
			p.chunkOf(ec).free(0, ei+1)
		}
	}
	p.update(base, npages, true, false)
}

const (
	pallocSumBytes = unsafe.Sizeof(pallocSum(0))

	// maxPackedValue is the maximum value that any of the three fields in
	// the pallocSum may take on.
	maxPackedValue    = 1 << logMaxPackedValue
	logMaxPackedValue = logPallocChunkPages + (summaryLevels-1)*summaryLevelBits

	freeChunkSum = pallocSum(uint64(pallocChunkPages) |
		uint64(pallocChunkPages<<logMaxPackedValue) |
		uint64(pallocChunkPages<<(2*logMaxPackedValue)))
)

// pallocSum is a packed summary type which packs three numbers: start, max,
// and end into a single 8-byte value. Each of these values are a summary of
// a bitmap and are thus counts, each of which may have a maximum value of
// 2^21 - 1, or all three may be equal to 2^21. The latter case is represented
// by just setting the 64th bit.
type pallocSum uint64

// packPallocSum takes a start, max, and end value and produces a pallocSum.
func packPallocSum(start, max, end uint) pallocSum {
	if max == maxPackedValue {
		// maxPackedValue=2^21
		// 将最高位设置为1
		return pallocSum(uint64(1 << 63))
	}
	return pallocSum((uint64(start) & (maxPackedValue - 1)) |
		((uint64(max) & (maxPackedValue - 1)) << logMaxPackedValue) |
		((uint64(end) & (maxPackedValue - 1)) << (2 * logMaxPackedValue)))
}

// start extracts the start value from a packed sum.
func (p pallocSum) start() uint {
	if uint64(p)&uint64(1<<63) != 0 {
		return maxPackedValue
	}
	return uint(uint64(p) & (maxPackedValue - 1))
}

// max extracts the max value from a packed sum.
func (p pallocSum) max() uint {
	if uint64(p)&uint64(1<<63) != 0 {
		return maxPackedValue
	}
	return uint((uint64(p) >> logMaxPackedValue) & (maxPackedValue - 1))
}

// end extracts the end value from a packed sum.
func (p pallocSum) end() uint {
	if uint64(p)&uint64(1<<63) != 0 {
		return maxPackedValue
	}
	return uint((uint64(p) >> (2 * logMaxPackedValue)) & (maxPackedValue - 1))
}

// unpack unpacks all three values from the summary.
func (p pallocSum) unpack() (uint, uint, uint) {
	if uint64(p)&uint64(1<<63) != 0 {
		return maxPackedValue, maxPackedValue, maxPackedValue
	}
	return uint(uint64(p) & (maxPackedValue - 1)),
		uint((uint64(p) >> logMaxPackedValue) & (maxPackedValue - 1)),
		uint((uint64(p) >> (2 * logMaxPackedValue)) & (maxPackedValue - 1))
}

// mergeSummaries merges consecutive summaries which may each represent at
// most 1 << logMaxPagesPerSum pages each together into one.
func mergeSummaries(sums []pallocSum, logMaxPagesPerSum uint) pallocSum {
	// Merge the summaries in sums into one.
	//
	// We do this by keeping a running summary representing the merged
	// summaries of sums[:i] in start, max, and end.
	start, max, end := sums[0].unpack()
	for i := 1; i < len(sums); i++ {
		// Merge in sums[i].
		si, mi, ei := sums[i].unpack()

		// Merge in sums[i].start only if the running summary is
		// completely free, otherwise this summary's start
		// plays no role in the combined sum.
		if start == uint(i)<<logMaxPagesPerSum {
			start += si
		}

		// Recompute the max value of the running sum by looking
		// across the boundary between the running sum and sums[i]
		// and at the max sums[i], taking the greatest of those two
		// and the max of the running sum.
		if end+si > max {
			max = end + si
		}
		if mi > max {
			max = mi
		}

		// Merge in end by checking if this new summary is totally
		// free. If it is, then we want to extend the running sum's
		// end by the new summary. If not, then we have some alloc'd
		// pages in there and we just want to take the end value in
		// sums[i].
		if ei == 1<<logMaxPagesPerSum {
			end += 1 << logMaxPagesPerSum
		} else {
			end = ei
		}
	}
	return packPallocSum(start, max, end)
}
