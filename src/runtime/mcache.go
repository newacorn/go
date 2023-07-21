// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// Per-thread (in Go, per-P) cache for small objects.
// This includes a small object cache and local allocation stats.
// No locking needed because it is per-thread (per-P).
//
// mcaches are allocated from non-GC'd memory, so any heap pointers
// must be specially handled.
// mheap 中的 mcentral 数组实现了全局范围的、基于 spanClass 的mspan管理
//
// 因为是全局的，所以需要加锁。
// 为了进一步减少锁竞争，GO把 mspan缓存到了每个P中，这就是 mcache。
// mcentral 提供了两个方法用来支持 mcache，一个是 cacheSpan()方法
// 它会分配一个span提供 mcache 使用，另一个是 uncacheSpan() 方法
// mcache 可以通过它把一个span归还给mcentral。
//
// 在Go的GMP模型中， mcache 是一个 per-P 的小对象缓存，因为每个P都有自己的一个本地
// mcache 所以不需要再加锁。mcache 结构也是在堆之外由专门的分配器分配的，所以不会被
// GC 扫描。
//
type mcache struct {
	_ sys.NotInHeap

	// The following members are accessed on every malloc,
	// so they are grouped here for better caching.
	// nextSample 配合 memory profile 来使用的，当开启 memory profile 的首，没
	// 分配 nextSample 这么多内存后，就会触发一次堆采样。
	nextSample uintptr // trigger heap sample after allocating this many bytes
	//
	// scanAlloc 记录的是从 mcache 总共分配了多少字节 scannable 类型的内存。
	// 其所属的obj是从是noscan 位为0，可以包含指针的 span中分配的。但是并不是将整个obj大小算作 scanable 类型的字节，
	// 而是这个obj分配给某个 _type 时， _type.ptrdata 的大小。
	//
	//  refill 函数返回之前，此字段会重置为0：
	// 	gcController.update(int64(s.npages*pageSize)-int64(usedBytes), int64(c.scanAlloc))
	// 	c.scanAlloc = 0
	scanAlloc  uintptr // bytes of scannable heap allocated

	// Allocator cache for tiny objects w/o pointers.
	// See "Tiny allocator" comment in malloc.go.

	// tiny points to the beginning of the current tiny block, or
	// nil if there is no current tiny block.
	//
	// tiny is a heap pointer. Since mcache is in non-GC'd memory,
	// we handle it by clearing it in releaseAll during mark
	// termination.
	//
	// tinyAllocs is the number of tiny allocations performed
	// by the P that owns this mcache.
	// tiny和tinyoffset 用来实现针对 noscan 型小对象的 tiny allocator，
	// tiny 指向一个16字节大小的内存单元
	tiny       uintptr
	// tinyoffset 记录的是这个内存单元中空闲的偏移量。
	tinyoffset uintptr

	// 记录的是总共进行了多少次 tiny 分配。
	tinyAllocs uintptr

	// The rest is not accessed on every malloc.

	// alloc 是根据 spanClass 缓存的一组 mspan，因为不需要加锁，所以不像
	// mcentral 那样对齐到 cache line。
	alloc [numSpanClasses]*mspan // spans to allocate from, indexed by spanClass

	// stackcache 是用来为 goroutine 分配栈的缓存。
	// _NumStackOrders = 4
	// 当本地 stackcache 中某个链表空了的时候， stackcacherefill()函数
	// 会循环调用 stackpoolalloc() 函数从 stackpool 中对应的链表中取一些
	// 节点过来不是按个数，这些节点大小总和为 _StackCacheSize 的一半，也就是每次16KB。
	// 所以 stackcache 链表中的栈并不是一定属于一个 mspan。
	//
	// 对应 2KB、4KB、8KB和16KB的栈链表。
	//
	// stackfreelist 链表中全部栈的大小不会超过32KB，当在 stackfree 函数中回收栈时，
	// 如果发现 stackfreelist 大小已经大于或等于32KB，会调用 stackcacherelease()
	// 进行释放只保留16KB大小总量，多余的栈释放到 stackpool 中。
	stackcache [_NumStackOrders]stackfreelist

	// flushGen indicates the sweepgen during which this mcache
	// was last flushed. If flushGen != mheap_.sweepgen, the spans
	// in this mcache are stale and need to the flushed so they
	// can be swept. This is done in acquirep.
	//
	// 记录的是上次执行 flush 时的 sweepgen ,如果不等于当前的 sweepgen
	// 就说明需要再次 flush 以进行清扫。
	flushGen atomic.Uint32
}

// A gclink is a node in a linked list of blocks, like mlink,
// but it is opaque to the garbage collector.
// The GC does not trace the pointers during collection,
// and the compiler does not emit write barriers for assignments
// of gclinkptr values. Code should store references to gclinks
// as gclinkptr, not as *gclink.
type gclink struct {
	next gclinkptr
}

// A gclinkptr is a pointer to a gclink, but it is opaque
// to the garbage collector.
type gclinkptr uintptr

// ptr returns the *gclink form of p.
// The result should be used for accessing fields, not stored
// in other data structures.
func (p gclinkptr) ptr() *gclink {
	return (*gclink)(unsafe.Pointer(p))
}

// 用来构建内存块链表，它会把每个节点最初的一个指针大小的内存用作
// 指向下一个节点的指针。
//
// 因为栈最小也有2KB，所以list字段可以很安全地基于 gclinkptr 把它们
// 连成一个链表，size字段用来标记链表的长度。
// 当本地 stackcache 中某个链表空了的时候，stackcacherefill() 函数
// 会循环调用 stackpollalloc() 函数从 stackpool 中对应的链表中取一些
// 节点过来不是按个数，而是按照空间大小为 _StackCacheSize的一半，也就是
// 每次16KB。
type stackfreelist struct {
	// 链表的头内存块的地址。其串联的是mspan中的obj，这些obj不一定属于同
	// 一个mspan，因为它们是一次一个从stackpool中申请的。
	//
	// 在 stackfree 函数中检测到链表总大小大于等于32KB，会调用 stackcacherelease()，
	// 将多余的栈释放到 stackpool 中，只保留16KB。
	list gclinkptr // linked list of free stacks
	// 链表的大小字节。
	// 每次分配栈时都会减少栈的大小。
	// 所以整个size是整个链表元素大小的整数倍。
	size uintptr   // total size of stacks in list
}

// dummy mspan that contains no free objects.
var emptymspan mspan

func allocmcache() *mcache {
	var c *mcache
	systemstack(func() {
		lock(&mheap_.lock)
		c = (*mcache)(mheap_.cachealloc.alloc())
		c.flushGen.Store(mheap_.sweepgen)
		unlock(&mheap_.lock)
	})
	for i := range c.alloc {
		c.alloc[i] = &emptymspan
	}
	c.nextSample = nextSample()
	return c
}

// freemcache releases resources associated with this
// mcache and puts the object onto a free list.
//
// In some cases there is no way to simply release
// resources, such as statistics, so donate them to
// a different mcache (the recipient).
func freemcache(c *mcache) {
	systemstack(func() {
		c.releaseAll()
		stackcache_clear(c)

		// NOTE(rsc,rlh): If gcworkbuffree comes back, we need to coordinate
		// with the stealing of gcworkbufs during garbage collection to avoid
		// a race where the workbuf is double-freed.
		// gcworkbuffree(c.gcworkbuf)

		lock(&mheap_.lock)
		mheap_.cachealloc.free(unsafe.Pointer(c))
		unlock(&mheap_.lock)
	})
}

// getMCache is a convenience function which tries to obtain an mcache.
//
// Returns nil if we're not bootstrapping or we don't have a P. The caller's
// P must not change, so we must be in a non-preemptible state.
func getMCache(mp *m) *mcache {
	// Grab the mcache, since that's where stats live.
	pp := mp.p.ptr()
	var c *mcache
	if pp == nil {
		// We will be called without a P while bootstrapping,
		// in which case we use mcache0, which is set in mallocinit.
		// mcache0 is cleared when bootstrapping is complete,
		// by procresize.
		c = mcache0
	} else {
		c = pp.mcache
	}
	return c
}

// refill acquires a new span of span class spc for c. This span will
// have at least one free object. The current span in c must be full.
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
func (c *mcache) refill(spc spanClass) {
	// Return the current cached span to the central lists.
	s := c.alloc[spc]

	// 一致性校验，mcache 要想从 mcentral 中获取一个新的 spc 规格的 mspan
	// 当前对应的 mspan 必要已经满了。
	if uintptr(s.allocCount) != s.nelems {
		throw("refill of span with free space remaining")
	}

	if s != &emptymspan {
		// Mark this span as no longer cached.
		if s.sweepgen != mheap_.sweepgen+3 {
			throw("bad sweepgen in refill")
		}
		// 将 s 归还给 mcentral
		mheap_.central[spc].mcentral.uncacheSpan(s)

		// Count up how many slots were used and record it.
		stats := memstats.heapStats.acquire()
		// slotsUsed 记录将此 mspan 分配给 mcache 之后
		// 到回收时被 mcache 用了多少个obj。
		slotsUsed := int64(s.allocCount) - int64(s.allocCountBeforeCache)
		// 更新全局的 smallAllocCount 计数。
		atomic.Xadd64(&stats.smallAllocCount[spc.sizeclass()], slotsUsed)

		// Flush tinyAllocs.
		// tinySpanClass = 5 objsize 16byte
		if spc == tinySpanClass {
			// 更新全局的 tinyAllocCount 计数
			atomic.Xadd64(&stats.tinyAllocCount, int64(c.tinyAllocs))
			c.tinyAllocs = 0
		}
		// 更新当前 P 的 statsSeq 字段的计数
		memstats.heapStats.release()

		// Count the allocs in inconsistent, internal stats.
		//
		// mcache从申请s到归还s,在s中消耗的所有obj的总字节数。
		bytesAllocated := slotsUsed * int64(s.elemsize)
		// 更新 gcController.totalAlloc 计数
		gcController.totalAlloc.Add(bytesAllocated)

		// Clear the second allocCount just to be safe.
		// 还回 s 之后，清空其 allocCountBeforeCache 字段。
		s.allocCountBeforeCache = 0
	}

	// Get a new cached span from the central lists.
	//
	// 根据 spc(spanClass) 从 mcentral 中申请一个至少有一个闲置
	// object 的 mspan。
	s = mheap_.central[spc].mcentral.cacheSpan()
	if s == nil {
		// 不能从 mcentral 中申请到mspan，说明内存已经消耗殆尽。
		// 抛出异常。
		throw("out of memory")
	}

	if uintptr(s.allocCount) == s.nelems {
		// 新申请的 mspan 必须至少有一个空闲的 object。
		throw("span has no free space")
	}

	// Indicate that this span is cached and prevent asynchronous
	// sweeping in the next sweep phase.
	s.sweepgen = mheap_.sweepgen + 3

	// Store the current alloc count for accounting later.
	//
	// 对于新申请的 mspan allocCountBeforeCache = allocCount
	s.allocCountBeforeCache = s.allocCount

	// Update heapLive and flush scanAlloc.
	//
	// We have not yet allocated anything new into the span, but we
	// assume that all of its slots will get used, so this makes
	// heapLive an overestimate.
	//
	// When the span gets uncached, we'll fix up this overestimate
	// if necessary (see releaseAll).
	//
	// We pick an overestimate here because an underestimate leads
	// the pacer to believe that it's in better shape than it is,
	// which appears to lead to more memory used. See #53738 for
	// more details.
	usedBytes := uintptr(s.allocCount) * s.elemsize
	//
	// int64(s.npages*pageSize)-int64(usedBytes) 是新申请的 mspan 中的
	// 空闲字节数。
	//
	// c.scanAlloc 记录的是从 mcache 总共分配了多少字节 scannable 类型的内存。
	// 其所属的obj是从是noscan 位为0，可以包含指针的 span中分配的。但是并不是将整个obj大小算作 scanable 类型的字节，
	// 而是这个obj分配给某个 _type 时， _type.ptrdata 的大小。
	//
	//  refill 函数返回之前，此字段会重置为0：
	// 	gcController.update(int64(s.npages*pageSize)-int64(usedBytes), int64(c.scanAlloc))
	// 	c.scanAlloc = 0
	gcController.update(int64(s.npages*pageSize)-int64(usedBytes), int64(c.scanAlloc))
	// 重置 c.scanAlloc
	c.scanAlloc = 0

	c.alloc[spc] = s
}

// allocLarge allocates a span for a large object.
func (c *mcache) allocLarge(size uintptr, noscan bool) *mspan {
	if size+_PageSize < size {
		throw("out of memory")
	}
	npages := size >> _PageShift
	if size&_PageMask != 0 {
		npages++
	}

	// Deduct credit for this span allocation and sweep if
	// necessary. mHeap_Alloc will also sweep npages, so this only
	// pays the debt down to npage pages.
	deductSweepCredit(npages*_PageSize, npages)

	spc := makeSpanClass(0, noscan)
	s := mheap_.alloc(npages, spc)
	if s == nil {
		throw("out of memory")
	}

	// Count the alloc in consistent, external stats.
	stats := memstats.heapStats.acquire()
	atomic.Xadd64(&stats.largeAlloc, int64(npages*pageSize))
	atomic.Xadd64(&stats.largeAllocCount, 1)
	memstats.heapStats.release()

	// Count the alloc in inconsistent, internal stats.
	gcController.totalAlloc.Add(int64(npages * pageSize))

	// Update heapLive.
	gcController.update(int64(s.npages*pageSize), 0)

	// Put the large span in the mcentral swept list so that it's
	// visible to the background sweeper.
	mheap_.central[spc].mcentral.fullSwept(mheap_.sweepgen).push(s)
	s.limit = s.base() + size
	s.initHeapBits(false)
	return s
}

func (c *mcache) releaseAll() {
	// Take this opportunity to flush scanAlloc.
	scanAlloc := int64(c.scanAlloc)
	c.scanAlloc = 0

	sg := mheap_.sweepgen
	dHeapLive := int64(0)
	for i := range c.alloc {
		s := c.alloc[i]
		if s != &emptymspan {
			slotsUsed := int64(s.allocCount) - int64(s.allocCountBeforeCache)
			s.allocCountBeforeCache = 0

			// Adjust smallAllocCount for whatever was allocated.
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.smallAllocCount[spanClass(i).sizeclass()], slotsUsed)
			memstats.heapStats.release()

			// Adjust the actual allocs in inconsistent, internal stats.
			// We assumed earlier that the full span gets allocated.
			gcController.totalAlloc.Add(slotsUsed * int64(s.elemsize))

			if s.sweepgen != sg+1 {
				print("----")
				// refill conservatively counted unallocated slots in gcController.heapLive.
				// Undo this.
				//
				// If this span was cached before sweep, then gcController.heapLive was totally
				// recomputed since caching this span, so we don't do this for stale spans.
				dHeapLive -= int64(uintptr(s.nelems)-uintptr(s.allocCount)) * int64(s.elemsize)
			}

			// Release the span to the mcentral.
			mheap_.central[i].mcentral.uncacheSpan(s)
			c.alloc[i] = &emptymspan
		}
	}
	// Clear tinyalloc pool.
	c.tiny = 0
	c.tinyoffset = 0

	// Flush tinyAllocs.
	stats := memstats.heapStats.acquire()
	atomic.Xadd64(&stats.tinyAllocCount, int64(c.tinyAllocs))
	c.tinyAllocs = 0
	memstats.heapStats.release()

	// Update heapLive and heapScan.
	gcController.update(dHeapLive, scanAlloc)
}

// prepareForSweep flushes c if the system has entered a new sweep phase
// since c was populated. This must happen between the sweep phase
// starting and the first allocation from c.
func (c *mcache) prepareForSweep() {
	// Alternatively, instead of making sure we do this on every P
	// between starting the world and allocating on that P, we
	// could leave allocate-black on, allow allocation to continue
	// as usual, use a ragged barrier at the beginning of sweep to
	// ensure all cached spans are swept, and then disable
	// allocate-black. However, with this approach it's difficult
	// to avoid spilling mark bits into the *next* GC cycle.
	sg := mheap_.sweepgen
	flushGen := c.flushGen.Load()
	if flushGen == sg {
		return
	} else if flushGen != sg-2 {
		println("bad flushGen", flushGen, "in prepareForSweep; sweepgen", sg)
		throw("bad flushGen")
	}
	c.releaseAll()
	stackcache_clear(c)
	c.flushGen.Store(mheap_.sweepgen) // Synchronizes with gcStart
}
