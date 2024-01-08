// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || arm64 || loong64 || mips64 || mips64le || ppc64 || ppc64le || riscv64 || s390x

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

const (
	// The number of levels in the radix tree.
	summaryLevels = 5

	// Constants for testing.
	pageAlloc32Bit = 0
	pageAlloc64Bit = 1

	// Number of bits needed to represent all indices into the L1 of the
	// chunks map.
	//
	// See (*pageAlloc).chunks for more details. Update the documentation
	// there should this number change.
	pallocChunksL1Bits = 13
)

// levelBits is the number of bits in the radix for a given level in the super summary
// structure.
//
// The sum of all the entries of levelBits should equal heapAddrBits.
var levelBits = [summaryLevels]uint{
	// 14
	summaryL0Bits,
	// 3
	summaryLevelBits,
	summaryLevelBits,
	summaryLevelBits,
	summaryLevelBits,
}

// levelShift is the number of bits to shift to acquire the radix for a given level
// in the super summary structure.
//
// With levelShift, one can compute the index of the summary at level l related to a
// pointer p by doing:
//
//	p >> levelShift[l]
var levelShift = [summaryLevels]uint{
	// 48-14=34 21+13(2^13=8KB一页)，此层级一个entry可以表示2^34个字节 = 16GB
	heapAddrBits - summaryL0Bits,
	// 48-14-3=31 18+13(2^13=8KB一页)，此层级一个entry可以表示2^31个字节 = 2GB
	heapAddrBits - summaryL0Bits - 1*summaryLevelBits,
	// 48-14-2*3=28 15+13(2^13=8KB一页)，此层级一个entry可以表示2^28个字节 = 256MB
	heapAddrBits - summaryL0Bits - 2*summaryLevelBits,
	// 48-14 -3*3=25 12+13(2^13=8KB一页)，此层级一个entry可以表示2^25个字节 = 32MB
	heapAddrBits - summaryL0Bits - 3*summaryLevelBits,
	// 48-14-4*3=22 9+13(2^13=8KB一页)，此层级一个entry可以表示2^22个字节 = 4MB
	heapAddrBits - summaryL0Bits - 4*summaryLevelBits,
}

// levelLogPages is log2 the maximum number of runtime pages in the address space
// a summary in the given level represents.
//
// The leaf level always represents exactly log2 of 1 chunk's worth of pages.
var levelLogPages = [summaryLevels]uint{
	// 21个bit位表示一个此层级的entry
	// 此层级的一个entry可表示2^21个页(8KB)
	logPallocChunkPages + 4*summaryLevelBits,
	// 18个bit位表示一个此层级的entry
	// 此层级的一个entry可表示2^18个页(8KB)
	logPallocChunkPages + 3*summaryLevelBits,
	// 15个bit位表示一个此层级的entry
	// 此层级的一个entry可表示2^15个页(8KB)
	logPallocChunkPages + 2*summaryLevelBits,
	// 12个bit位表示一个此层级的entry
	// 此层级的一个entry可表示2^12个页(8KB)
	logPallocChunkPages + 1*summaryLevelBits,
	// 9个bit位表示一个此层级的entry。
	// 此层级的一个entry可表示2^9个页(8KB)
	logPallocChunkPages,
}

// sysInit performs architecture-dependent initialization of fields
// in pageAlloc. pageAlloc should be uninitialized except for sysStat
// if any runtime statistic should be updated.
func (p *pageAlloc) sysInit() {
	// Reserve memory for each level. This will get mapped in
	// as R/W by setArenas.
	for l, shift := range levelShift {
		// 34 31 28 25 22
		//entries=2^14,2^17,2^20,2^23,2^26
		entries := 1 << (heapAddrBits - shift)

		// Reserve b bytes of memory anywhere in the address space.
		// b = 2^14*8,2^17*8,2^20*8,2^23*8,2^26*8
		// b表示每个层级用于存储元数据所需的字节数。
		// 比如最后一层，每个entry可以表4MB但总共有2^48字节的地址空间所以需要，2^48/4M=2^26个entry，
		// 每个entry需要一个pallocSumBytes类型
		// 的值用于保存页使用情况的元数据。所以最后一层需要2^26*pallocSumBytes(8字节的uint64)个字节来
		// 保存元数据。
		b := alignUp(uintptr(entries)*pallocSumBytes, physPageSize)
		r := sysReserve(nil, b)
		if r == nil {
			throw("failed to reserve page summary memory")
		}

		// Put this reservation into a slice.
		sl := notInHeapSlice{(*notInHeap)(r), 0, entries}
		p.summary[l] = *(*[]pallocSum)(unsafe.Pointer(&sl))
	}

	// Set up the scavenge index.
	nbytes := uintptr(1<<heapAddrBits) / pallocChunkBytes / 8
	r := sysReserve(nil, nbytes)
	sl := notInHeapSlice{(*notInHeap)(r), int(nbytes), int(nbytes)}
	p.scav.index.chunks = *(*[]atomic.Uint8)(unsafe.Pointer(&sl))
}

// sysGrow performs architecture-dependent operations on heap
// growth for the page allocator, such as mapping in new memory
// for summaries. It also updates the length of the slices in
// [.summary.
//
// base is the base of the newly-added heap memory and limit is
// the first address past the end of the newly-added heap memory.
// Both must be aligned to pallocChunkBytes.
//
// The caller must update p.start and p.end after calling sysGrow.
//
// 为[base,limit)这段地址区间的各种元数据信息所在的结构分配内存。
//
// 根据base，limit计算出 pageAlloc.summary 和 pageAlloc.scav.index.chunks对应的元素，
// 将 pageAlloc.summary 中这些元素和pageAlloc.scav.index.chunks 中这些元素的地址空间
// 变成reay状态。
//
// base,limit 地址区间[base,limit)。
func (p *pageAlloc) sysGrow(base, limit uintptr) {
	if base%pallocChunkBytes != 0 || limit%pallocChunkBytes != 0 {
		print("runtime: base = ", hex(base), ", limit = ", hex(limit), "\n")
		throw("sysGrow bounds not aligned to pallocChunkBytes")
	}

	// addrRangeToSummaryRange converts a range of addresses into a range
	// of summary indices which must be mapped to support those addresses
	// in the summary range.
	addrRangeToSummaryRange := func(level int, r addrRange) (int, int) {
		// level=[0,4]
		//
		// level级别的entry索引，sumIdxBase表示起始索引包括，sumIdxLimit表示结束索引(不包括)。
		// [sumIdxBase,sumIdxLimit)表示r表示的地址区间在lvel级别的元数据信息分布的entry的索引。
		sumIdxBase, sumIdxLimit := addrsToSummaryRange(level, r.base.addr(), r.limit.addr())
		//before  blockAlignSummaryRange
		/**
		level 8240 8241
		level 65920 65921
		level 527360 527361
		level 4218880 4218881
		level 33751040 33751041
		*/
		//after  blockAlignSummaryRange
		/**
		after align 0 16384
		after align 65920 65928
		after align 527360 527368
		after align 4218880 4218888
		after align 33751040 33751048
		*/
		return blockAlignSummaryRange(level, sumIdxBase, sumIdxLimit)
	}

	// summaryRangeToSumAddrRange converts a range of indices in any
	// level of p.summary into page-aligned addresses which cover that
	// range of indices.
	//
	// 一个地址对应一个pallocSum类型的值。
	// 即解引用这个地址可以得到对这个entry的元数据信息。
	summaryRangeToSumAddrRange := func(level, sumIdxBase, sumIdxLimit int) addrRange {
		baseOffset := alignDown(uintptr(sumIdxBase)*pallocSumBytes, physPageSize)
		limitOffset := alignUp(uintptr(sumIdxLimit)*pallocSumBytes, physPageSize)
		base := unsafe.Pointer(&p.summary[level][0])
		return addrRange{
			offAddr{uintptr(add(base, baseOffset))},
			offAddr{uintptr(add(base, limitOffset))},
		}
	}

	// addrRangeToSumAddrRange is a convienience function that converts
	// an address range r to the address range of the given summary level
	// that stores the summaries for r.
	addrRangeToSumAddrRange := func(level int, r addrRange) addrRange {
		sumIdxBase, sumIdxLimit := addrRangeToSummaryRange(level, r)
		return summaryRangeToSumAddrRange(level, sumIdxBase, sumIdxLimit)
	}

	// Find the first inUse index which is strictly greater than base.
	//
	// Because this function will never be asked remap the same memory
	// twice, this index is effectively the index at which we would insert
	// this new growth, and base will never overlap/be contained within
	// any existing range.
	//
	// This will be used to look at what memory in the summary array is already
	// mapped before and after this new range.
	//
	// inUseIndex 是 p.inUse 中第一个其base字段比base大的addrRange的索引。
	// inUseIndex的特殊值为0和len(p.inUse)+1。
	inUseIndex := p.inUse.findSucc(base)
	// 第一次调用 inUseIndex是0，因为p.inUse还没有初始化。
	// println("inUseIndex",inUseIndex)
	// 0

	// Walk up the radix tree and map summaries in as needed.
	for l := range p.summary {
		// Figure out what part of the summary array this new address space needs.
		needIdxBase, needIdxLimit := addrRangeToSummaryRange(l, makeAddrRange(base, limit))

		/**
		needIdxBase, needIdxLimit
		after align 0 16384
		after align 65920 65928
		after align 527360 527368
		after align 4218880 4218888
		after align 33751040 33751048
		*/
		// Update the summary slices with a new upper-bound. This ensures
		// we get tight bounds checks on at least the top bound.
		//
		// We must do this regardless of whether we map new memory.
		if needIdxLimit > len(p.summary[l]) {
			p.summary[l] = p.summary[l][:needIdxLimit]
		}

		//
		// Compute the needed address range in the summary array for level l.
		need := summaryRangeToSumAddrRange(l, needIdxBase, needIdxLimit)
		// println("after summaryRangeToSumAddrRange",need.base.a,need.limit.a)
		/**
		after summaryRangeToSumAddrRange 19554304 19685376
		after summaryRangeToSumAddrRange 21495808 21499904
		after summaryRangeToSumAddrRange 37773312 37777408
		after summaryRangeToSumAddrRange 75694080 75698176
		after summaryRangeToSumAddrRange 379060224 379064320
		*/
		// Prune need down to what needs to be newly mapped. Some parts of it may
		// already be mapped by what inUse describes due to page alignment requirements
		// for mapping. prune's invariants are guaranteed by the fact that this
		// function will never be asked to remap the same memory twice.
		if inUseIndex > 0 {
			need = need.subtract(addrRangeToSumAddrRange(l, p.inUse.ranges[inUseIndex-1]))
		}
		if inUseIndex < len(p.inUse.ranges) {
			need = need.subtract(addrRangeToSumAddrRange(l, p.inUse.ranges[inUseIndex]))
		}
		// It's possible that after our pruning above, there's nothing new to map.
		if need.size() == 0 {
			continue
		}
		// println("need.size()",need.size())
		// 第一次调用：
		/**
		need.size() 131072
		need.size() 4096
		need.size() 4096
		need.size() 4096
		need.size() 4096
		*/

		// Map and commit need.
		// 下面map的是各级别保存元数据信息所需的内存地址空间。即各个pallocSum实例的内存需求。
		sysMap(unsafe.Pointer(need.base.addr()), need.size(), p.sysStat)
		sysUsed(unsafe.Pointer(need.base.addr()), need.size(), need.size())
		p.summaryMappedReady += need.size()
	}

	// println("base,limit",base,limit)
	// base,limit 824633720832 824637915136 ,base-limt=4MB
	// Update the scavenge index.
	p.summaryMappedReady += p.scav.index.grow(base, limit, p.sysStat)
}

// grow increases the index's backing store in response to a heap growth.
// Returns the amount of memory added to sysStat.
//
// 根据base,limit找到对应的chunk，再找到对应的s.chunks的成员，
// 为 scavengeIndex.chunks 的这些成员地址执行sysMap/sysUsed，即将它们的地址区间变为ready状态。
// 并按条件更新 scavengeIndex.min 和 scavengeIndex.max
func (s *scavengeIndex) grow(base, limit uintptr, sysStat *sysMemStat) uintptr {
	if base%pallocChunkBytes != 0 || limit%pallocChunkBytes != 0 {
		print("runtime: base = ", hex(base), ", limit = ", hex(limit), "\n")
		throw("sysGrow bounds not aligned to pallocChunkBytes")
	}
	// Map and commit the pieces of chunks that we need.
	//
	// We always map the full range of the minimum heap address to the
	// maximum heap address. We don't do this for the summary structure
	// because it's quite large and a discontiguous heap could cause a
	// lot of memory to be used. In this situation, the worst case overhead
	// is in the single-digit MiB if we map the whole thing.
	//
	// The base address of the backing store is always page-aligned,
	// because it comes from the OS, so it's sufficient to align the
	// index.
	haveMin := s.min.Load()
	haveMax := s.max.Load()
	// 一个索引对应一个字节，包含8位对应8个chunk。
	needMin := int32(alignDown(uintptr(chunkIndex(base)/8), physPageSize))
	needMax := int32(alignUp(uintptr((chunkIndex(limit)+7)/8), physPageSize))
	// 第一次调用
	// println("--",uintptr(chunkIndex(base)),uintptr(chunkIndex(limit)))
	// -- 33751040 33751041
	//
	// println("needMin","needMax",needMin,needMax)
	// needMin-needMax=4218880-4222976=4096
	//
	// Extend the range down to what we have, if there's no overlap.
	if needMax < haveMin {
		needMax = haveMin
	}
	if needMin > haveMax {
		needMin = haveMax
	}
	have := makeAddrRange(
		// Avoid a panic from indexing one past the last element.
		uintptr(unsafe.Pointer(&s.chunks[0]))+uintptr(haveMin),
		uintptr(unsafe.Pointer(&s.chunks[0]))+uintptr(haveMax),
	)
	need := makeAddrRange(
		// Avoid a panic from indexing one past the last element.
		uintptr(unsafe.Pointer(&s.chunks[0]))+uintptr(needMin),
		uintptr(unsafe.Pointer(&s.chunks[0]))+uintptr(needMax),
	)
	// Subtract any overlap from rounding. We can't re-map memory because
	// it'll be zeroed.
	need = need.subtract(have)

	// If we've got something to map, map it, and update the slice bounds.
	if need.size() != 0 {
		sysMap(unsafe.Pointer(need.base.addr()), need.size(), sysStat)
		sysUsed(unsafe.Pointer(need.base.addr()), need.size(), need.size())
		// Update the indices only after the new memory is valid.
		if haveMin == 0 || needMin < haveMin {
			s.min.Store(needMin)
		}
		if haveMax == 0 || needMax > haveMax {
			s.max.Store(needMax)
		}
	}
	// Update minHeapIdx. Note that even if there's no mapping work to do,
	// we may still have a new, lower minimum heap address.
	//
	// minHeapIdx: 实际存在的prepared的地址区间的起始地址所在的chunk在
	// s.chunks 中的索引。
	// s.minHeapIdx 存储的是这些索中的最小值。
	minHeapIdx := s.minHeapIdx.Load()
	if baseIdx := int32(chunkIndex(base) / 8); minHeapIdx == 0 || baseIdx < minHeapIdx {
		s.minHeapIdx.Store(baseIdx)
	}
	return need.size()
}
