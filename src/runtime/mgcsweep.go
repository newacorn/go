// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector: sweeping

// The sweeper consists of two different algorithms:
//
// * The object reclaimer finds and frees unmarked slots in spans. It
//   can free a whole span if none of the objects are marked, but that
//   isn't its goal. This can be driven either synchronously by
//   mcentral.cacheSpan for mcentral spans, or asynchronously by
//   sweepone, which looks at all the mcentral lists.
//
// * The span reclaimer looks for spans that contain no marked objects
//   and frees whole spans. This is a separate algorithm because
//   freeing whole spans is the hardest task for the object reclaimer,
//   but is critical when allocating new spans. The entry point for
//   this is mheap_.reclaim and it's driven by a sequential scan of
//   the page marks bitmap in the heap arenas.
//
// Both algorithms ultimately call mspan.sweep, which sweeps a single
// heap span.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

var sweep sweepdata

// State of background sweep.
type sweepdata struct {
	lock   mutex
	g      *g
	parked bool

	nbgsweep    uint32
	npausesweep uint32

	// active tracks outstanding sweepers and the sweep
	// termination condition.
	active activeSweep

	// centralIndex is the current unswept span class.
	// It represents an index into the mcentral span
	// sets. Accessed and updated via its load and
	// update methods. Not protected by a lock.
	//
	// Reset at mark termination.
	// Used by mheap.nextSpanForSweep.
	centralIndex sweepClass
}

// sweepClass is a spanClass and one bit to represent whether we're currently
// sweeping partial or full spans.
type sweepClass uint32

const (
	numSweepClasses            = numSpanClasses * 2
	sweepClassDone  sweepClass = sweepClass(^uint32(0))
)

func (s *sweepClass) load() sweepClass {
	return sweepClass(atomic.Load((*uint32)(s)))
}

func (s *sweepClass) update(sNew sweepClass) {
	// Only update *s if its current value is less than sNew,
	// since *s increases monotonically.
	sOld := s.load()
	for sOld < sNew && !atomic.Cas((*uint32)(s), uint32(sOld), uint32(sNew)) {
		sOld = s.load()
	}
	// TODO(mknyszek): This isn't the only place we have
	// an atomic monotonically increasing counter. It would
	// be nice to have an "atomic max" which is just implemented
	// as the above on most architectures. Some architectures
	// like RISC-V however have native support for an atomic max.
}

func (s *sweepClass) clear() {
	atomic.Store((*uint32)(s), 0)
}

// split returns the underlying span class as well as
// whether we're interested in the full or partial
// unswept lists for that class, indicated as a boolean
// (true means "full").
func (s sweepClass) split() (spc spanClass, full bool) {
	return spanClass(s >> 1), s&1 == 0
}

// nextSpanForSweep finds and pops the next span for sweeping from the
// central sweep buffers. It returns ownership of the span to the caller.
// Returns nil if no such span exists.
func (h *mheap) nextSpanForSweep() *mspan {
	sg := h.sweepgen
	for sc := sweep.centralIndex.load(); sc < numSweepClasses; sc++ {
		spc, full := sc.split()
		c := &h.central[spc].mcentral
		var s *mspan
		if full {
			s = c.fullUnswept(sg).pop()
		} else {
			s = c.partialUnswept(sg).pop()
		}
		if s != nil {
			// Write down that we found something so future sweepers
			// can start from here.
			sweep.centralIndex.update(sc)
			return s
		}
	}
	// Write down that we found nothing.
	sweep.centralIndex.update(sweepClassDone)
	return nil
}

const sweepDrainedMask = 1 << 31

// activeSweep is a type that captures whether sweeping
// is done, and whether there are any outstanding sweepers.
//
// Every potential sweeper must call begin() before they look
// for work, and end() after they've finished sweeping.
type activeSweep struct {
	// state is divided into two parts.
	//
	// The top bit (masked by sweepDrainedMask) is a boolean
	// value indicating whether all the sweep work has been
	// drained from the queue.
	//
	// The rest of the bits are a counter, indicating the
	// number of outstanding concurrent sweepers.
	state atomic.Uint32
}

// begin registers a new sweeper. Returns a sweepLocker
// for acquiring spans for sweeping. Any outstanding sweeper blocks
// sweep termination.
//
// If the sweepLocker is invalid, the caller can be sure that all
// outstanding sweep work has been drained, so there is nothing left
// to sweep. Note that there may be sweepers currently running, so
// this does not indicate that all sweeping has completed.
//
// Even if the sweepLocker is invalid, its sweepGen is always valid.
func (a *activeSweep) begin() sweepLocker {
	for {
		state := a.state.Load()
		if state&sweepDrainedMask != 0 {
			return sweepLocker{mheap_.sweepgen, false}
		}
		if a.state.CompareAndSwap(state, state+1) {
			return sweepLocker{mheap_.sweepgen, true}
		}
	}
}

// end deregisters a sweeper. Must be called once for each time
// begin is called if the sweepLocker is valid.
func (a *activeSweep) end(sl sweepLocker) {
	if sl.sweepGen != mheap_.sweepgen {
		throw("sweeper left outstanding across sweep generations")
	}
	for {
		state := a.state.Load()
		if (state&^sweepDrainedMask)-1 >= sweepDrainedMask {
			throw("mismatched begin/end of activeSweep")
		}
		if a.state.CompareAndSwap(state, state-1) {
			if state != sweepDrainedMask {
				return
			}
			if debug.gcpacertrace > 0 {
				live := gcController.heapLive.Load()
				print("pacer: sweep done at heap size ", live>>20, "MB; allocated ", (live-mheap_.sweepHeapLiveBasis)>>20, "MB during sweep; swept ", mheap_.pagesSwept.Load(), " pages at ", mheap_.sweepPagesPerByte, " pages/byte\n")
			}
			return
		}
	}
}

// markDrained marks the active sweep cycle as having drained
// all remaining work. This is safe to be called concurrently
// with all other methods of activeSweep, though may race.
//
// Returns true if this call was the one that actually performed
// the mark.
func (a *activeSweep) markDrained() bool {
	for {
		state := a.state.Load()
		if state&sweepDrainedMask != 0 {
			return false
		}
		if a.state.CompareAndSwap(state, state|sweepDrainedMask) {
			return true
		}
	}
}

// sweepers returns the current number of active sweepers.
func (a *activeSweep) sweepers() uint32 {
	return a.state.Load() &^ sweepDrainedMask
}

// isDone returns true if all sweep work has been drained and no more
// outstanding sweepers exist. That is, when the sweep phase is
// completely done.
func (a *activeSweep) isDone() bool {
	return a.state.Load() == sweepDrainedMask
}

// reset sets up the activeSweep for the next sweep cycle.
//
// The world must be stopped.
func (a *activeSweep) reset() {
	assertWorldStopped()
	a.state.Store(0)
}

// finishsweep_m ensures that all spans are swept.
//
// The world must be stopped. This ensures there are no sweeps in
// progress.
// finishsweep_m() 函数目前只被 gcStart 函数调用。
// 此函数在 STW 状态下被调用。
// 1. 确保上一轮GC清扫任务肯定全部完成。
// 2. 重置 mcentral 中的 full/partial unSwept spanSet。
// 3. 唤醒 scavenger 。
// 4. gcBitsArenas 中字段的角色互换和回收。
//
//go:nowritebarrier
func finishsweep_m() {
	assertWorldStopped()

	// Sweeping must be complete before marking commences, so
	// sweep any unswept spans. If this is a concurrent GC, there
	// shouldn't be any spans left to sweep, so this should finish
	// instantly.
	//
	// If GC was forced before the concurrent sweep
	// finished, there may be spans to sweep.
	//
	// 因为此时处于STW状态，所以当工作队列中没有任务时便是所有的清扫任务都完成了。
	// 前提是正在执行sweepone的协程不能被抢占。下面的断言也验证了这点。
	for sweepone() != ^uintptr(0) {
		sweep.npausesweep++
	}

	// Make sure there aren't any outstanding sweepers left.
	// At this point, with the world stopped, it means one of two
	// things.
	// Either we were able to preempt a sweeper, or that
	// a sweeper didn't call sweep.active.end when it should have.
	// Both cases indicate a bug, so throw.
	if sweep.active.sweepers() != 0 {
		throw("active sweepers found at start of mark phase")
	}

	// Reset all the unswept buffers, which should be empty.
	// Do this in sweep termination as opposed to mark termination
	// so that we can catch unswept spans and reclaim blocks as
	// soon as possible[因为spanset类型共有一个全局的spanSetBlocks pool].
	//
	// // spanSetBlockPool is a global pool of spanSetBlocks.
	// var spanSetBlockPool spanSetBlockAlloc
	sg := mheap_.sweepgen
	for i := range mheap_.central {
		c := &mheap_.central[i].mcentral
		// 下面两个函数执行之后，它们对应的2个spanset都处于初始状态，不包含任何msapn.
		c.partialUnswept(sg).reset()
		c.fullUnswept(sg).reset()
	}

	// Sweeping is done, so if the scavenger isn't already awake,
	// wake it up. There's definitely work for it to do at this
	// point.
	//
	// sweep之后，一般会存在完全释放的msapn，所以很可能会有可scavenge的页面。
	// 唤醒后台scavenger协程，按需回收页归还操作系统。
	scavenger.wake()

	nextMarkBitArenaEpoch()
}

func bgsweep(c chan int) {
	sweep.g = getg()

	lockInit(&sweep.lock, lockRankSweep)
	lock(&sweep.lock)
	sweep.parked = true
	c <- 1
	goparkunlock(&sweep.lock, waitReasonGCSweepWait, traceEvGoBlock, 1)

	for {
		// bgsweep attempts to be a "low priority" goroutine by intentionally
		// yielding time. It's OK if it doesn't run, because goroutines allocating
		// memory will sweep and ensure that all spans are swept before the next
		// GC cycle. We really only want to run when we're idle.
		//
		// However, calling Gosched after each span swept produces a tremendous
		// amount of tracing events, sometimes up to 50% of events in a trace. It's
		// also inefficient to call into the scheduler so much because sweeping a
		// single span is in general a very fast operation, taking as little as 30 ns
		// on modern hardware. (See #54767.)
		//
		// As a result, bgsweep sweeps in batches, and only calls into the scheduler
		// at the end of every batch. Furthermore, it only yields its time if there
		// isn't spare idle time available on other cores. If there's available idle
		// time, helping to sweep can reduce allocation latencies by getting ahead of
		// the proportional sweeper and having spans ready to go for allocation.
		const sweepBatchSize = 10
		nSwept := 0
		for sweepone() != ^uintptr(0) {
			sweep.nbgsweep++
			nSwept++
			if nSwept%sweepBatchSize == 0 {
				goschedIfBusy()
			}
		}
		for freeSomeWbufs(true) {
			// N.B. freeSomeWbufs is already batched internally.
			goschedIfBusy()
		}
		lock(&sweep.lock)
		if !isSweepDone() {
			// This can happen if a GC runs between
			// gosweepone returning ^0 above
			// and the lock being acquired.
			unlock(&sweep.lock)
			continue
		}
		sweep.parked = true
		goparkunlock(&sweep.lock, waitReasonGCSweepWait, traceEvGoBlock, 1)
	}
}

// sweepLocker acquires sweep ownership of spans.
type sweepLocker struct {
	// sweepGen is the sweep generation of the heap.
	sweepGen uint32
	valid    bool
}

// sweepLocked represents sweep ownership of a span.
type sweepLocked struct {
	*mspan
}

// tryAcquire attempts to acquire sweep ownership of span s. If it
// successfully acquires ownership, it blocks sweep completion.
func (l *sweepLocker) tryAcquire(s *mspan) (sweepLocked, bool) {
	if !l.valid {
		throw("use of invalid sweepLocker")
	}
	// Check before attempting to CAS.
	if atomic.Load(&s.sweepgen) != l.sweepGen-2 {
		return sweepLocked{}, false
	}
	// Attempt to acquire sweep ownership of s.
	if !atomic.Cas(&s.sweepgen, l.sweepGen-2, l.sweepGen-1) {
		return sweepLocked{}, false
	}
	return sweepLocked{s}, true
}

// sweepone sweeps some unswept heap span and returns the number of pages returned
// to the heap, or ^uintptr(0) if there was nothing to sweep.
//
// 返回^uintptr(0)，表示任务池没有任务可取，但其它并行工作者可能还在执行清理工作。
func sweepone() uintptr {
	gp := getg()

	// Increment locks to ensure that the goroutine is not preempted
	// in the middle of sweep thus leaving the span in an inconsistent state for next GC
	gp.m.locks++

	// TODO(austin): sweepone is almost always called in a loop;
	// lift the sweepLocker into its callers.
	sl := sweep.active.begin()
	if !sl.valid {
		gp.m.locks--
		return ^uintptr(0)
	}

	// Find a span to sweep.
	npages := ^uintptr(0)
	var noMoreWork bool
	for {
		s := mheap_.nextSpanForSweep()
		if s == nil {
			noMoreWork = sweep.active.markDrained()
			break
		}
		if state := s.state.get(); state != mSpanInUse {
			// This can happen if direct sweeping already
			// swept this span, but in that case the sweep
			// generation should always be up-to-date.
			if !(s.sweepgen == sl.sweepGen || s.sweepgen == sl.sweepGen+3) {
				print("runtime: bad span s.state=", state, " s.sweepgen=", s.sweepgen, " sweepgen=", sl.sweepGen, "\n")
				throw("non in-use span in unswept list")
			}
			continue
		}
		if s, ok := sl.tryAcquire(s); ok {
			// Sweep the span we found.
			npages = s.npages
			if s.sweep(false) {
				// Whole span was freed. Count it toward the
				// page reclaimer credit since these pages can
				// now be used for span allocation.
				mheap_.reclaimCredit.Add(npages)
			} else {
				// Span is still in-use, so this returned no
				// pages to the heap and the span needs to
				// move to the swept in-use list.
				npages = 0
			}
			break
		}
	}
	sweep.active.end(sl)

	if noMoreWork {
		// The sweep list is empty. There may still be
		// concurrent sweeps running, but we're at least very
		// close to done sweeping.

		// Move the scavenge gen forward (signaling
		// that there's new work to do) and wake the scavenger.
		//
		// The scavenger is signaled by the last sweeper because once
		// sweeping is done, we will definitely have useful work for
		// the scavenger to do, since the scavenger only runs over the
		// heap once per GC cycle. This update is not done during sweep
		// termination because in some cases there may be a long delay
		// between sweep done and sweep termination (e.g. not enough
		// allocations to trigger a GC) which would be nice to fill in
		// with scavenging work.
		if debug.scavtrace > 0 {
			systemstack(func() {
				lock(&mheap_.lock)
				released := atomic.Loaduintptr(&mheap_.pages.scav.released)
				printScavTrace(released, false)
				atomic.Storeuintptr(&mheap_.pages.scav.released, 0)
				unlock(&mheap_.lock)
			})
		}
		scavenger.ready()
	}

	gp.m.locks--
	return npages
}

// isSweepDone reports whether all spans are swept.
//
// Note that this condition may transition from false to true at any
// time as the sweeper runs. It may transition from true to false if a
// GC runs; to prevent that the caller must be non-preemptible or must
// somehow block GC progress.
func isSweepDone() bool {
	return sweep.active.isDone()
}

// Returns only when span s has been swept.
//
//go:nowritebarrier
func (s *mspan) ensureSwept() {
	// Caller must disable preemption.
	// Otherwise when this function returns the span can become unswept again
	// (if GC is triggered on another goroutine).
	gp := getg()
	if gp.m.locks == 0 && gp.m.mallocing == 0 && gp != gp.m.g0 {
		throw("mspan.ensureSwept: m is not locked")
	}

	// If this operation fails, then that means that there are
	// no more spans to be swept. In this case, either s has already
	// been swept, or is about to be acquired for sweeping and swept.
	sl := sweep.active.begin()
	if sl.valid {
		// The caller must be sure that the span is a mSpanInUse span.
		if s, ok := sl.tryAcquire(s); ok {
			s.sweep(false)
			sweep.active.end(sl)
			return
		}
		sweep.active.end(sl)
	}

	// Unfortunately we can't sweep the span ourselves. Somebody else
	// got to it first. We don't have efficient means to wait, but that's
	// OK, it will be swept fairly soon.
	for {
		spangen := atomic.Load(&s.sweepgen)
		if spangen == sl.sweepGen || spangen == sl.sweepGen+3 {
			break
		}
		osyield()
	}
}

// Sweep frees or collects finalizers for blocks not marked in the mark phase.
// It clears the mark bits in preparation for the next GC round.
// Returns true if the span was returned to heap.
// If preserve=true, don't return it to heap nor relink in mcentral lists;
// caller takes care of it.
//
// 根据preserve的值分两种情况处理：
// 1. preserve=true，只清扫 sweepLocked.mspan 并不会将它放置在某处。
// 2. preserve=false:
// 2.1. 有对象被标记：
// 如果对象全都被标记则将其放入 mcentral.full 的fullsweept队列中。
// 部分被标记将其放入 mcentral.partial 的partialsweept队列中。
// 2.2. 没有对象被标记：
// 将此mspan管理的内存对应的地址区间在 pageAlloc.chunks 中的对应的alloc位/scavenged位
// 分别置为0和1。并更新在 pageAlloc.summary 中对应的各个层级的sum信息。
// 并未对此地址空间执行 sysUnused 调用。
//
// 结果：
// 如果没有被归还到页堆，则 mspan.sweepgen = mheap.sweepgen
// mspan.allocBits = mspan.gcmarkBits
// mspan.allocCount = mspan总被标记的对象。
// mspan.allocCache 根据新的allocBits重置。
// mspan.freeindex = 0
// mspan.freeIndexForScan = 0
//
func (sl *sweepLocked) sweep(preserve bool) bool {
	// It's critical that we enter this function with preemption disabled,
	// GC must not start while we are in the middle of this function.
	gp := getg()
	if gp.m.locks == 0 && gp.m.mallocing == 0 && gp != gp.m.g0 {
		throw("mspan.sweep: m is not locked")
	}

	s := sl.mspan
	if !preserve {
		// We'll release ownership of this span. Nil it out to
		// prevent the caller from accidentally using it.
		sl.mspan = nil
	}

	sweepgen := mheap_.sweepgen
	if state := s.state.get(); state != mSpanInUse || s.sweepgen != sweepgen-1 {
		print("mspan.sweep: state=", state, " sweepgen=", s.sweepgen, " mheap.sweepgen=", sweepgen, "\n")
		throw("mspan.sweep: bad span state")
	}

	if trace.enabled {
		traceGCSweepSpan(s.npages * _PageSize)
	}

	mheap_.pagesSwept.Add(int64(s.npages))

	spc := s.spanclass
	size := s.elemsize

	// The allocBits indicate which unmarked objects don't need to be
	// processed since they were free at the end of the last GC cycle
	// and were not allocated since then.
	// If the allocBits index is >= s.freeindex and the bit
	// is not marked then the object remains unallocated
	// since the last GC.
	// This situation is analogous to being on a freelist.

	// Unlink & free special records for any objects we're about to free.
	// Two complications here:
	// 1. An object can have both finalizer and profile special records.
	//    In such case we need to queue finalizer for execution,
	//    mark the object as live and preserve the profile special.
	// 2. A tiny object can have several finalizers setup for different offsets.
	//    If such object is not marked, we need to queue all finalizers at once.
	// Both 1 and 2 are possible at the same time.
	hadSpecials := s.specials != nil
	siter := newSpecialsIter(s)
	for siter.valid() {
		// 一次处理一个obj上所有的special，如果此obj包含spcial的话。
		//
		// 1. 如果此对象没有被标记：
		// 1.1 将对像关联了_KindSpecialFinalizer类型的spcials移除，但保留其它类型spcials。
		// 并将此对象标记为存活。
		// 1.2 此队列没有关联_KindSpecialFinalizer类型的special，则将其关联的所有specals都移除。
		// 2. 如果此对象被标记了：
		// 将对象关联的 _KindSpecialReachable 类相等special移除，但保留其它类型的spcials。
		//
		// 被移除的spcial除了从mspan.specials链表中移除的话，还对它们执行
		// freeSpecial()调用。
		//
		// A finalizer can be set for an inner byte of an object, find object beginning.
		// 当前special所关联的obj在msapn中的索引。
		objIndex := uintptr(siter.s.offset) / size
		// p表示obj的起始地址。
		p := s.base() + objIndex*size
		mbits := s.markBitsForIndex(objIndex)
		if !mbits.isMarked() {
			// 当前special关联的obj没有被标记为存活。
			//
			// This object is not marked and has at least one special record.
			// Pass 1: see if it has at least one finalizer.
			hasFin := false
			endOffset := p - s.base() + size
			// obj末端地址在msapn中的偏移量。
			for tmp := siter.s; tmp != nil && uintptr(tmp.offset) < endOffset; tmp = tmp.next {
				// 一个obj可能关联多个special或者一个obj是tinyObj里面的多个小对象关联了special。
				if tmp.kind == _KindSpecialFinalizer {
					// 凡是此obj/或其子对象关联了 _KindSpecialFinalizer 则将其标记为存活。
					//
					// Stop freeing of object if it has a finalizer.
					mbits.setMarkedNonAtomic()
					hasFin = true
					break
				}
			}
			// Pass 2: queue all finalizers _or_ handle profile record.
			for siter.valid() && uintptr(siter.s.offset) < endOffset {
				// Find the exact byte for which the special was setup
				// (as opposed to object beginning).
				special := siter.s
				p := s.base() + uintptr(special.offset)
				if special.kind == _KindSpecialFinalizer || !hasFin {
					// 要么只移除此对象关联的所有 _kindSpecialFinalizer类型的spcials，
					// 要么移除此对象关联的所有spcials(此对象没有关联fin类型的spcial)。
					siter.unlinkAndNext()
					freeSpecial(special, unsafe.Pointer(p), size)
				} else {
					// The object has finalizers, so we're keeping it alive.
					// All other specials only apply when an object is freed,
					// so just keep the special record.
					siter.next()
				}
			}
		} else {
			// object is still live
			if siter.s.kind == _KindSpecialReachable {
				special := siter.unlinkAndNext()
				(*specialReachable)(unsafe.Pointer(special)).reachable = true
				freeSpecial(special, unsafe.Pointer(p), size)
			} else {
				// keep special record
				siter.next()
			}
		}
	}
	if hadSpecials && s.specials == nil {
		// 当s中存在specials且在上面的操作中s中所有spcial都被移除了。
		// 将s所在的arena关联的heapArena中specials位图中对应的位置为0。
		spanHasNoSpecials(s)
	}

	if debug.allocfreetrace != 0 || debug.clobberfree != 0 || raceenabled || msanenabled || asanenabled {
		// Find all newly freed objects. This doesn't have to
		// efficient; allocfreetrace has massive overhead.
		mbits := s.markBitsForBase()
		abits := s.allocBitsForIndex(0)
		for i := uintptr(0); i < s.nelems; i++ {
			if !mbits.isMarked() && (abits.index < s.freeindex || abits.isMarked()) {
				x := s.base() + i*s.elemsize
				if debug.allocfreetrace != 0 {
					tracefree(unsafe.Pointer(x), size)
				}
				if debug.clobberfree != 0 {
					clobberfree(unsafe.Pointer(x), size)
				}
				// User arenas are handled on explicit free.
				if raceenabled && !s.isUserArenaChunk {
					racefree(unsafe.Pointer(x), size)
				}
				if msanenabled && !s.isUserArenaChunk {
					msanfree(unsafe.Pointer(x), size)
				}
				if asanenabled && !s.isUserArenaChunk {
					asanpoison(unsafe.Pointer(x), size)
				}
			}
			mbits.advance()
			abits.advance()
		}
	}

	// Check for zombie objects.
	//
	// 从s.freeindex开始的所有对象，如果存在某些对象被GC标记了但没有被allocBits记录则
	// 调用s.reportZombies报告。
	if s.freeindex < s.nelems {
		// Everything < freeindex is allocated and hence
		// cannot be zombies.
		//
		// Check the first bitmap byte, where we have to be
		// careful with freeindex.
		obj := s.freeindex
		if (*s.gcmarkBits.bytep(obj / 8)&^*s.allocBits.bytep(obj / 8))>>(obj%8) != 0 {
			//s.freeindex处的对象被GC标记但allocBits位并没有被设置。
			s.reportZombies()
		}
		// Check remaining bytes.
		// 检查剩余s.freeindex之后的对象。
		for i := obj/8 + 1; i < divRoundUp(s.nelems, 8); i++ {
			if *s.gcmarkBits.bytep(i)&^*s.allocBits.bytep(i) != 0 {
				s.reportZombies()
			}
		}
	}

	// Count the number of free objects in this span.
	//
	// nalloc s中被标记的对象个数，这些对象在sweep后会被保留。
	nalloc := uint16(s.countAlloc())
	// nfreed s中被释放的对象。
	nfreed := s.allocCount - nalloc
	if nalloc > s.allocCount {
		// 被标记的对象的数量不可能大于被分配的对象数量。
		//
		// The zombie check above should have caught this in
		// more detail.
		print("runtime: nelems=", s.nelems, " nalloc=", nalloc, " previous allocCount=", s.allocCount, " nfreed=", nfreed, "\n")
		throw("sweep increased allocation count")
	}

	// 更新s的一些字段。
	s.allocCount = nalloc
	s.freeindex = 0 // reset allocation index to start of span.
	s.freeIndexForScan = 0
	if trace.enabled {
		getg().m.p.ptr().traceReclaimed += uintptr(nfreed) * s.elemsize
	}

	// gcmarkBits becomes the allocBits.
	// get a fresh cleared gcmarkBits in preparation for next GC
	//
	// 置换标记位和分配位。
	s.allocBits = s.gcmarkBits
	s.gcmarkBits = newMarkBits(s.nelems)

	// Initialize alloc bits cache.
	//
	// 按新的 allocBits 来设置 s.allocCache。
	s.refillAllocCache(0)

	// The span must be in our exclusive ownership until we update sweepgen,
	// check for potential races.
	if state := s.state.get(); state != mSpanInUse || s.sweepgen != sweepgen-1 {
		// mSpanInUse 状态的mspan才能被扫描，清扫状态中的s，其s.sweepgen=mheap_.sweepgen-1。
		print("mspan.sweep: state=", state, " sweepgen=", s.sweepgen, " mheap.sweepgen=", sweepgen, "\n")
		throw("mspan.sweep: bad span state after sweep")
	}
	if s.sweepgen == sweepgen+1 || s.sweepgen == sweepgen+3 {
		// 如果mspan的sweepgen字段的值是这两种情况，那么它们都是被缓存在某个p的mcache中。
		// 其中 s.sweepgen == sweepgen+3 不应该被清扫。
		// s.sweepgen == sweepgen+1 需要将 s.swwepgen=mheap_.sweepgen-1，才能进行清扫。
		throw("swept cached span")
	}

	// We need to set s.sweepgen = h.sweepgen only when all blocks are swept,
	// because of the potential for a concurrent free/SetFinalizer.
	//
	// But we need to set it before we make the span available for allocation
	// (return it to heap or mcentral), because allocation code assumes that a
	// span is already swept if available for allocation.
	//
	// Serialization point.
	// At this point the mark bits are cleared and allocation ready
	// to go so release the span.
	atomic.Store(&s.sweepgen, sweepgen)

	if s.isUserArenaChunk {
		if preserve {
			// This is a case that should never be handled by a sweeper that
			// preserves the span for reuse.
			throw("sweep: tried to preserve a user arena span")
		}
		if nalloc > 0 {
			// There still exist pointers into the span or the span hasn't been
			// freed yet. It's not ready to be reused. Put it back on the
			// full swept list for the next cycle.
			mheap_.central[spc].mcentral.fullSwept(sweepgen).push(s)
			return false
		}

		// It's only at this point that the sweeper doesn't actually need to look
		// at this arena anymore, so subtract from pagesInUse now.
		mheap_.pagesInUse.Add(-s.npages)
		s.state.set(mSpanDead)

		// The arena is ready to be recycled. Remove it from the quarantine list
		// and place it on the ready list. Don't add it back to any sweep lists.
		systemstack(func() {
			// It's the arena code's responsibility to get the chunk on the quarantine
			// list by the time all references to the chunk are gone.
			if s.list != &mheap_.userArena.quarantineList {
				throw("user arena span is on the wrong list")
			}
			lock(&mheap_.lock)
			mheap_.userArena.quarantineList.remove(s)
			mheap_.userArena.readyList.insert(s)
			unlock(&mheap_.lock)
		})
		return false
	}

	if spc.sizeclass() != 0 {
		// Handle spans for small objects.
		//
		// mspan.elemsize<=32KB的
		// spanclass=[1,68]
		if nfreed > 0 {
			// Only mark the span as needing zeroing if we've freed any
			// objects, because a fresh span that had been allocated into,
			// wasn't totally filled, but then swept, still has all of its
			// free slots zeroed.
			s.needzero = 1
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.smallFreeCount[spc.sizeclass()], int64(nfreed))
			memstats.heapStats.release()

			// Count the frees in the inconsistent, internal stats.
			gcController.totalFree.Add(int64(nfreed) * int64(s.elemsize))
		}
		if !preserve {
			// The caller may not have removed this span from whatever
			// unswept set its on but taken ownership of the span for
			// sweeping by updating sweepgen. If this span still is in
			// an unswept set, then the mcentral will pop it off the
			// set, check its sweepgen, and ignore it.
			//
			// preserve=fase，表示清理之后，可以按条件将其放到正确位置。
			if nalloc == 0 {
				// s在清扫之后完全没有存活对象了。
				// 将其放回页堆。将来从页中分配堆时可以用到。
				//
				// Free totally free span directly back to the heap.
				mheap_.freeSpan(s)
				return true
			}
			// Return span back to the right mcentral list.
			if uintptr(nalloc) == s.nelems {
				// 在清扫之后，s中都是存活对象。
				// 即此msapn在下一轮GC执行清扫之前不可能再被使用了(1.因为已经清扫过了，2.因为没有空闲对象可供分配)。
				mheap_.central[spc].mcentral.fullSwept(sweepgen).push(s)
			} else {
				// 在清扫之后，s中存在空闲对象。
				mheap_.central[spc].mcentral.partialSwept(sweepgen).push(s)
			}
		}
	} else if !preserve {
		// preserve=fase，表示清理之后，可以按条件将其放到正确位置。
		//
		// 这里的路基处理的s，其管理的内存大于32KB。
		// 即s.elemsize>32KB，这样的mspan只有一个对象。
		// Handle spans for large objects.
		if nfreed != 0 {
			// 因为大mspan只有一个obj，所以nfreed=0表示整个mspan管理的内存都是空的。
			// nfreed肯定等于1。
			//
			// Free large object span to heap.

			// NOTE(rsc,dvyukov): The original implementation of efence
			// in CL 22060046 used sysFree instead of sysFault, so that
			// the operating system would eventually give the memory
			// back to us again, so that an efence program could run
			// longer without running out of memory. Unfortunately,
			// calling sysFree here without any kind of adjustment of the
			// heap data structures means that when the memory does
			// come back to us, we have the wrong metadata for it, either in
			// the mspan structures or in the garbage collection bitmap.
			// Using sysFault here means that the program will run out of
			// memory fairly quickly in efence mode, but at least it won't
			// have mysterious crashes due to confused memory reuse.
			// It should be possible to switch back to sysFree if we also
			// implement and then call some kind of mheap.deleteSpan.
			if debug.efence > 0 {
				s.limit = 0 // prevent mlookup from finding this span
				sysFault(unsafe.Pointer(s.base()), size)
			} else {
				// 将mspan管理的内存释放到堆中。
				// preserve=fase且
				// 没有存活对象的大对象mspan将其放到页堆中。
				mheap_.freeSpan(s)
			}

			// Count the free in the consistent, external stats.
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.largeFreeCount, 1)
			atomic.Xadd64(&stats.largeFree, int64(size))
			memstats.heapStats.release()

			// Count the free in the inconsistent, internal stats.
			gcController.totalFree.Add(int64(size))

			return true
		}

		// Add a large span directly onto the full+swept list.
		//
		// 将此大对象存活且需要我们处理，所以添加到fullSwept列表中去。
		mheap_.central[spc].mcentral.fullSwept(sweepgen).push(s)
	}
	return false
}

// reportZombies reports any marked but free objects in s and throws.
//
// This generally means one of the following:
//
// 1. User code converted a pointer to a uintptr and then back
// unsafely, and a GC ran while the uintptr was the only reference to
// an object.
//
// 2. User code (or a compiler bug) constructed a bad pointer that
// points to a free slot, often a past-the-end pointer.
//
// 3. The GC two cycles ago missed a pointer and freed a live object,
// but it was still live in the last cycle, so this GC cycle found a
// pointer to that object and marked it.
func (s *mspan) reportZombies() {
	printlock()
	print("runtime: marked free object in span ", s, ", elemsize=", s.elemsize, " freeindex=", s.freeindex, " (bad use of unsafe.Pointer? try -d=checkptr)\n")
	mbits := s.markBitsForBase()
	abits := s.allocBitsForIndex(0)
	for i := uintptr(0); i < s.nelems; i++ {
		addr := s.base() + i*s.elemsize
		print(hex(addr))
		alloc := i < s.freeindex || abits.isMarked()
		if alloc {
			print(" alloc")
		} else {
			print(" free ")
		}
		if mbits.isMarked() {
			print(" marked  ")
		} else {
			print(" unmarked")
		}
		zombie := mbits.isMarked() && !alloc
		if zombie {
			print(" zombie")
		}
		print("\n")
		if zombie {
			length := s.elemsize
			if length > 1024 {
				length = 1024
			}
			hexdumpWords(addr, addr+length, nil)
		}
		mbits.advance()
		abits.advance()
	}
	throw("found pointer to free object")
}

// deductSweepCredit deducts sweep credit for allocating a span of
// size spanBytes. This must be performed *before* the span is
// allocated to ensure the system has enough credit. If necessary, it
// performs sweeping to prevent going in to debt. If the caller will
// also sweep pages (e.g., for a large allocation), it can pass a
// non-zero callerSweepPages to leave that many pages unswept.
//
// deductSweepCredit makes a worst-case assumption that all spanBytes
// bytes of the ultimately allocated span will be available for object
// allocation.
//
// deductSweepCredit is the core of the "proportional sweep" system.
// It uses statistics gathered by the garbage collector to perform
// enough sweeping so that all pages are swept during the concurrent
// sweep phase between GC cycles.
//
// mheap_ must NOT be locked.
//
// 协程在分配内存时，进行辅助sweep。
// 在 mcache.allocLarge 和 mcache.refill 调用中会调用此函数。
func deductSweepCredit(spanBytes uintptr, callerSweepPages uintptr) {
	if mheap_.sweepPagesPerByte == 0 {
		// Proportional sweep is done or disabled.
		return
	}

	if trace.enabled {
		traceGCSweepStart()
	}

	// Fix debt if necessary.
retry:
	sweptBasis := mheap_.pagesSweptBasis.Load()
	live := gcController.heapLive.Load()
	liveBasis := mheap_.sweepHeapLiveBasis
	newHeapLive := spanBytes
	if liveBasis < live {
		// Only do this subtraction when we don't overflow. Otherwise, pagesTarget
		// might be computed as something really huge, causing us to get stuck
		// sweeping here until the next mark phase.
		//
		// Overflow can happen here if gcPaceSweeper is called concurrently with
		// sweeping (i.e. not during a STW, like it usually is) because this code
		// is intentionally racy. A concurrent call to gcPaceSweeper can happen
		// if a GC tuning parameter is modified and we read an older value of
		// heapLive than what was used to set the basis.
		//
		// This state should be transient, so it's fine to just let newHeapLive
		// be a relatively small number. We'll probably just skip this attempt to
		// sweep.
		//
		// See issue #57523.
		newHeapLive += uintptr(live - liveBasis)
	}
	pagesTarget := int64(mheap_.sweepPagesPerByte*float64(newHeapLive)) - int64(callerSweepPages)
	for pagesTarget > int64(mheap_.pagesSwept.Load()-sweptBasis) {
		if sweepone() == ^uintptr(0) {
			mheap_.sweepPagesPerByte = 0
			break
		}
		if mheap_.pagesSweptBasis.Load() != sweptBasis {
			// Sweep pacing changed. Recompute debt.
			goto retry
		}
	}

	if trace.enabled {
		traceGCSweepDone()
	}
}

// clobberfree sets the memory content at x to bad content, for debugging
// purposes.
func clobberfree(x unsafe.Pointer, size uintptr) {
	// size (span.elemsize) is always a multiple of 4.
	for i := uintptr(0); i < size; i += 4 {
		*(*uint32)(add(x, i)) = 0xdeadbeef
	}
}

// gcPaceSweeper updates the sweeper's pacing parameters.
//
// Must be called whenever the GC's pacing is updated.
//
// The world must be stopped, or mheap_.lock must be held.
func gcPaceSweeper(trigger uint64) {
	assertWorldStoppedOrLockHeld(&mheap_.lock)

	// Update sweep pacing.
	if isSweepDone() {
		mheap_.sweepPagesPerByte = 0
	} else {
		// Concurrent sweep needs to sweep all of the in-use
		// pages by the time the allocated heap reaches the GC
		// trigger. Compute the ratio of in-use pages to sweep
		// per byte allocated, accounting for the fact that
		// some might already be swept.
		heapLiveBasis := gcController.heapLive.Load()
		heapDistance := int64(trigger) - int64(heapLiveBasis)
		// Add a little margin so rounding errors and
		// concurrent sweep are less likely to leave pages
		// unswept when GC starts.
		heapDistance -= 1024 * 1024
		if heapDistance < _PageSize {
			// Avoid setting the sweep ratio extremely high
			heapDistance = _PageSize
		}
		pagesSwept := mheap_.pagesSwept.Load()
		pagesInUse := mheap_.pagesInUse.Load()
		sweepDistancePages := int64(pagesInUse) - int64(pagesSwept)
		if sweepDistancePages <= 0 {
			mheap_.sweepPagesPerByte = 0
		} else {
			mheap_.sweepPagesPerByte = float64(sweepDistancePages) / float64(heapDistance)
			mheap_.sweepHeapLiveBasis = heapLiveBasis
			// Write pagesSweptBasis last, since this
			// signals concurrent sweeps to recompute
			// their debt.
			mheap_.pagesSweptBasis.Store(pagesSwept)
		}
	}
}
