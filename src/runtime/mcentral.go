// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Central free lists.
//
// See malloc.go for an overview.
//
// The mcentral doesn't actually contain the list of free objects; the mspan does.
// Each mcentral is two lists of mspans: those with free objects (c->nonempty)
// and those that are completely allocated (c->empty).

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
)

// Central list of free objects of a given size.
// 一个 mcentral 类型的对象，对应一种 spanClass ,管理着一组属于该
// spanClass 的 mspan。
//
type mcentral struct {
	_         sys.NotInHeap
	// spanclass 记录当前 mcntral 管理着哪种类型的 mspan。
	// spanClass 取值[0-136]
	// (67+1)*2=136
	// 额外一个是一个大小为0的sizeclass。
	// 每个class都包括两个类型的spanClass，前一个索引用于分配
	// 有指针的内存，后一个索引用于分配没有指针的内存。
	spanclass spanClass

	// partial and full contain two mspan sets: one of swept in-use
	// spans, and one of unswept in-use spans. These two trade
	// roles on each GC cycle. The unswept set is drained either by
	// allocation or by the background sweeper in every GC cycle,
	// so only two roles are necessary.
	//
	// sweepgen is increased by 2 on each GC cycle, so the swept
	// spans are in partial[sweepgen/2%2] and the unswept spans are in
	// partial[1-sweepgen/2%2]. Sweeping pops spans from the
	// unswept set and pushes spans that are still in-use on the
	// swept set. Likewise, allocating an in-use span pushes it
	// on the swept set.
	//
	// Some parts of the sweeper can sweep arbitrary spans, and hence
	// can't remove them from the unswept set, but will add the span
	// to the appropriate swept list. As a result, the parts of the
	// sweeper and mcentral that do consume from the unswept list may
	// encounter swept spans, and these should be ignored.
	//
	// partial 和 full 的类型是 [2]spanSet 数组， spanSet 有自己的锁，是个并发安全地支持push和pop的*mspan集合。
	//
	// 有一个包含的是已清扫的 mspan，另一个包含的是未清扫的 mspan。对应索引并不固定。
	// 它们在每轮 GC的标记终止阶段(mehap_.sweepgen递增2之后)会互换角色。
	//
	// 每轮 sweep 开始时，随之 mheap.sweepgen 的递增 2，unSweept spanSet 会与 weept spanSet 进行替换。
	//
	// 因为通过调用 partialUnswept 和 fullUnswept 函数来决定 partial 和 full 这两个数组中哪个是 unSwept spanSet 哪个不是。
	// 而这两个函数在计算数组索引时依赖 mheap.sweepgen 的值。
	//
	// *存在两个spanSet的根本原因是，在清扫其中一个spanSet时，被清扫过的mspan如果不想放回页堆，就必须放在另外一个不同的spanSet队列。
	// *具体解释见 partialUnswept fullUnswept 函数的注释。
	// -----------------
	//
	// partial 中的是至少包含一个空闲对象的 mspan。
	partial [2]spanSet // list of spans with a free object
	//  full 中都是mspan都是没有空闲对象的mspan，但是在清扫阶段会发生弹出式清扫。
	//
	// 上一轮GC标记终止之后(mheap_.sweepgen递增2之后)到此轮GC标记终止之前(mheap_.sweepgen递增2之前)期间，
	// fullSwept链表(标记结束前其身份是fullUnSwept)由空到填充()。fullUnSwept链表(其中的mspan经历上一轮GC
	// 的标记(标记结束前期身份是fullSwept)和清扫，在清扫结束时发生了重置)由非空变为空。
	full    [2]spanSet // list of spans with no free objects
}

// Initialize a single central free list.
func (c *mcentral) init(spc spanClass) {
	c.spanclass = spc
	lockInit(&c.partial[0].spineLock, lockRankSpanSetSpine)
	lockInit(&c.partial[1].spineLock, lockRankSpanSetSpine)
	lockInit(&c.full[0].spineLock, lockRankSpanSetSpine)
	lockInit(&c.full[1].spineLock, lockRankSpanSetSpine)
}

// partialUnswept returns the spanSet which holds partially-filled
// unswept spans for this sweepgen.
//
// 此列表是由partialSwept列表在此轮GC标记结束时(mheap_.sweepgen递增2后)转换而来的
// 里面包含了一些在此轮GC看来已经标记但还未清扫的mspan。
//
// 在此轮GC清扫阶段会对此列表中的msapn进行清扫并重置此列表。
// 即此列表在此轮GC标记结束之后到下一轮GC标记结束之后都是空的。
func (c *mcentral) partialUnswept(sweepgen uint32) *spanSet {
	return &c.partial[1-sweepgen/2%2]
}

// partialSwept returns the spanSet which holds partially-filled
// swept spans for this sweepgen.
//
// 此列表是在本轮GC标记结束时(mheap_.sweepgen)由partialUnswept列表转换而来的。
// 转换前此列表为空，在此轮GC清扫阶段进行填充。
//
// mspan清扫之后存在一些空闲对象。这样的msapn可以在接下来分配obj时被用到。
func (c *mcentral) partialSwept(sweepgen uint32) *spanSet {
	return &c.partial[sweepgen/2%2]
}

// fullUnswept returns the spanSet which holds unswept spans without any
// free slots for this sweepgen.
//
// 下面说的此轮指sweepgen对应的GC轮。
// 此列表不是直接填充的，而是由fullSwept列表在下一轮GC标记结束时(mheap_.sweepgen的值发生了变更)
// 转换过来的。
//
// 这里的mspan对象都被分配了，且被此轮GC标记阶段标记了，但对象标记情况不确定。
// 在此轮GC清扫阶段会清扫其中所有的msapn。在清扫结束时将此列表重置为空。
//
// 即此列表在上一轮的清扫结束后到此轮GC标记刚刚结束前(mheap_.sweepgen发生变更之前)，此列表一直都是空的。
func (c *mcentral) fullUnswept(sweepgen uint32) *spanSet {
	return &c.full[1-sweepgen/2%2]
}

// fullSwept returns the spanSet which holds swept spans without any
// free slots for this sweepgen.
//
// 此列表是由此轮GC标记结束时(mheap_.sweepgen递增2)后，由fullUnSwept列表转换(转换时此列表为空)而来的。
// 在此轮GC清扫阶段进行填充。
//
// 此列表是本轮GC清扫阶段在清扫mspan时，由那些所有对象都被标记的mspan所填充。
// 所以此列表中的msapn在下一轮GC执行清扫之前不可能再被使用了(1.因为已经清扫过了，2.因为没有空闲对象可供分配)。
func (c *mcentral) fullSwept(sweepgen uint32) *spanSet {
	return &c.full[sweepgen/2%2]
}

// Allocate a span to use in an mcache.
func (c *mcentral) cacheSpan() *mspan {
	// Deduct credit for this span allocation and sweep if necessary.
	//
	// 当前 spanclass 对应的 mspan 占用的页面包含的总字节数
	spanBytes := uintptr(class_to_allocnpages[c.spanclass.sizeclass()]) * _PageSize
	// 按需进行辅助sweep。
	deductSweepCredit(spanBytes, 0)

	traceDone := false
	if trace.enabled {
		traceGCSweepStart()
	}

	// If we sweep spanBudget spans without finding any free
	// space, just allocate a fresh span. This limits the amount
	// of time we can spend trying to find free space and
	// amortizes the cost of small object sweeping over the
	// benefit of having a full free span to allocate from. By
	// setting this to 100, we limit the space overhead to 1%.
	//
	// TODO(austin,mknyszek): This still has bad worst-case
	// throughput. For example, this could find just one free slot
	// on the 100th swept span. That limits allocation latency, but
	// still has very poor throughput. We could instead keep a
	// running free-to-used budget and switch to fresh span
	// allocation if the budget runs low.
	spanBudget := 100

	var s *mspan
	var sl sweepLocker

	// Try partial swept spans first.
	// 先查看 mcentral 中的 partial mspan列表中是否有
	// 带空闲obj且已经扫描过的 msapn。
	sg := mheap_.sweepgen
	if s = c.partialSwept(sg).pop(); s != nil {
		// s中含有空闲的obj且已经被扫描过了。
		// 已找到符合条件的 msapn 跳转到 hevesan 标签
		// 继续后续的处理。
		goto havespan
	}

	sl = sweep.active.begin()
	if sl.valid {
		// Now try partial unswept spans.
		for ; spanBudget >= 0; spanBudget-- {
			s = c.partialUnswept(sg).pop()
			if s == nil {
				break
			}
			if s, ok := sl.tryAcquire(s); ok {
				// We got ownership of the span, so let's sweep it and use it.
				s.sweep(true)
				sweep.active.end(sl)
				goto havespan
			}
			// We failed to get ownership of the span, which means it's being or
			// has been swept by an asynchronous sweeper that just couldn't remove it
			// from the unswept list.
			// That sweeper took ownership of the span and
			// responsibility for either freeing it to the heap or putting it on the
			// right swept list. Either way, we should just ignore it (and it's unsafe
			// for us to do anything else).
		}
		// Now try full unswept spans, sweeping them and putting them into the
		// right list if we fail to get a span.
		for ; spanBudget >= 0; spanBudget-- {
			s = c.fullUnswept(sg).pop()
			if s == nil {
				break
			}
			if s, ok := sl.tryAcquire(s); ok {
				// We got ownership of the span, so let's sweep it.
				s.sweep(true)
				// Check if there's any free space.
				freeIndex := s.nextFreeIndex()
				if freeIndex != s.nelems {
					s.freeindex = freeIndex
					sweep.active.end(sl)
					goto havespan
				}
				// Add it to the swept list, because sweeping didn't give us any free space.
				c.fullSwept(sg).push(s.mspan)
			}
			// See comment for partial unswept spans.
		}
		sweep.active.end(sl)
	}
	if trace.enabled {
		traceGCSweepDone()
		traceDone = true
	}

	// We failed to get a span from the mcentral so get one from mheap.
	s = c.grow()
	if s == nil {
		return nil
	}

	// At this point s is a span that should have free slots.
havespan:
	if trace.enabled && !traceDone {
		traceGCSweepDone()
	}
	n := int(s.nelems) - int(s.allocCount)
	if n == 0 || s.freeindex == s.nelems || uintptr(s.allocCount) == s.nelems {
		throw("span has no free objects")
	}
	freeByteBase := s.freeindex &^ (64 - 1)
	// s.freeindex 目前对应 s.allocBits 中的元素的索引。
	// s.freeindex 每递增64便会对应一个新的s.allocBits数组元素。
	whichByte := freeByteBase / 8
	// Init alloc bits cache.
	s.refillAllocCache(whichByte)

	// Adjust the allocCache so that s.freeindex corresponds to the low bit in
	// s.allocCache.
	// 更新 s.allocCache 使其最低位与s.freeindex对应。
	s.allocCache >>= s.freeindex % 64

	return s
}

// Return span from an mcache.
//
// s must have a span class corresponding to this
// mcentral and it must not be empty(因为p上的mcache不会主动填充，只有在真正需要时才会填充，所以必定有被使用的对象).
//
// 根据s.sweepgen对s进行清扫或者方法mcentral的swept队列中。
//
// 参数：
// 有三种种状态的s:
// 1. p中缓存已标记未清扫的mspan，在GC终止阶段结束后所有的p在用于分配内存之前会调用
// prepareForSweep 方法(调用此函数)释放其缓存的所有mspan(这样的mspan具有mspan.sweepgen=mheap_.sweepgen+1)。
// * 会被清扫
// 2. 在使用p上的缓存进行分配内存时，如果发现p中的某个类型的mspanClass对应的mspan上没有空闲对象了。会调用refill方
// 法，其会调用uncacheSpan释放该mspan(这样的mspan具有mspan.sweepgen=mheap_.sweepgen+3，mspa没有空闲对象)。
// 3. p复用时(aquirep())也会出现调用 prepareForSweep -> uncacheSpan 的情况。这种情况下mspan=swepgen+3。
// * 2/3类型的msapn会根据是否有空闲对象放入到partialSwept/fullSwept队列。
func (c *mcentral) uncacheSpan(s *mspan) {
	if s.allocCount == 0 {
		throw("uncaching span but s.allocCount == 0")
	}

	sg := mheap_.sweepgen
	stale := s.sweepgen == sg+1

	// Fix up sweepgen.
	if stale {
		// 标记终止阶段mheap_.sweepgen递增2导致mspan与其差值为1。
		// 标记终止结束后，所有p缓存的msapn接下来不会用于分配内存，而是
		// 直接调用uncacheSpan函数进行释放。见prepareForSweep方法。
		//
		// 这种msapn是已标记未清扫的。因为其在标记终止阶段随着(mheap_.sweepgen递增2)
		// 产生的。
		//
		// Span was cached before sweep began. It's our
		// responsibility to sweep it.
		//
		// Set sweepgen to indicate it's not cached but needs
		// sweeping and can't be allocated from. sweep will
		// set s.sweepgen to indicate s is swept.
		//
		// 由 sg+1 -> sg-1
		atomic.Store(&s.sweepgen, sg-1)
	} else {
		// Indicate that s is no longer cached.
		//
		// 由 sg+3 -> sg
		atomic.Store(&s.sweepgen, sg)
	}

	// Put the span in the appropriate place.
	if stale {
		// It's stale, so just sweep it. Sweeping will put it on
		// the right list.
		//
		// We don't use a sweepLocker here. Stale cached spans
		// aren't in the global sweep lists, so mark termination
		// itself holds up sweep completion until all mcaches
		// have been swept.
		ss := sweepLocked{s}
		ss.sweep(false)
	} else {
		// s.sweepgen=mheap_.sweepgen+3，
		// 所以它们不需要被清扫，将它们放入到swept队列中。
		// 在mheap_.sweepgen递增时，swept队列会变成unSwept队列会被清扫。
		if int(s.nelems)-int(s.allocCount) > 0 {
			// Put it back on the partial swept list.
			c.partialSwept(sg).push(s)
		} else {
			// There's no free space and it's not stale, so put it on the
			// full swept list.
			c.fullSwept(sg).push(s)
		}
	}
}

// grow allocates a new empty span from the heap and initializes it for c's size class.
func (c *mcentral) grow() *mspan {
	// npages: c.spanclass 类型的msapn占用的页数。
	npages := uintptr(class_to_allocnpages[c.spanclass.sizeclass()])
	// size: c.spanclass类型mspan中obj的大小。
	size := uintptr(class_to_size[c.spanclass.sizeclass()])

	s := mheap_.alloc(npages, c.spanclass)
	if s == nil {
		return nil
	}

	// Use division by multiplication and shifts to quickly compute:
	// n := (npages << _PageShift) / size
	n := s.divideByElemSize(npages << _PageShift)
	s.limit = s.base() + size*n
	s.initHeapBits(false)
	return s
}
