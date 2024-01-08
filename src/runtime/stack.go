// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/cpu"
	"internal/goarch"
	"internal/goos"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

/*
Stack layout parameters.
Included both by runtime (compiled via 6c) and linkers (compiled via gcc).

The per-goroutine g->stackguard is set to point StackGuard bytes
above the bottom of the stack.  Each function compares its stack
pointer against g->stackguard to check for overflow.  To cut one
instruction from the check sequence for functions with tiny frames,
the stack is allowed to protrude StackSmall bytes below the stack
guard.  Functions with large frames don't bother with the check and
always call morestack.  The sequences are (for amd64, others are
similar):

	guard = g->stackguard
	frame = function's stack frame size
	argsize = size of function arguments (call + return)

	stack frame size <= StackSmall:
		CMPQ guard, SP
		JHI 3(PC)
		MOVQ m->morearg, $(argsize << 32)
		CALL morestack(SB)

	stack frame size > StackSmall but < StackBig
		LEAQ (frame-StackSmall)(SP), R0
		CMPQ guard, R0
		JHI 3(PC)
		MOVQ m->morearg, $(argsize << 32)
		CALL morestack(SB)

	stack frame size >= StackBig:
		MOVQ m->morearg, $((argsize << 32) | frame)
		CALL morestack(SB)

The bottom StackGuard - StackSmall bytes are important: there has
to be enough room to execute functions that refuse to check for
stack overflow, either because they need to be adjacent to the
actual caller's frame (deferproc) or because they handle the imminent
stack overflow (morestack).

For example, deferproc might call malloc, which does one of the
above checks (without allocating a full frame), which might trigger
a call to morestack.  This sequence needs to fit in the bottom
section of the stack.  On amd64, morestack's frame is 40 bytes, and
deferproc's frame is 56 bytes.  That fits well within the
StackGuard - StackSmall bytes at the bottom.
The linkers explore all possible call traces involving non-splitting
functions to make sure that this limit cannot be violated.
*/

const (
	// StackSystem is a number of additional bytes to add
	// to each stack below the usual guard area for OS-specific
	// purposes like signal handling. Used on Windows, Plan 9,
	// and iOS because they do not use a separate stack.
	_StackSystem = goos.IsWindows*512*goarch.PtrSize + goos.IsPlan9*512 + goos.IsIos*goarch.IsArm64*1024

	// The minimum size of stack used by Go code
	_StackMin = 2048

	// The minimum stack size to allocate.
	// The hackery here rounds FixedStack0 up to a power of 2.
	// 2048
	_FixedStack0 = _StackMin + _StackSystem
	// 2047
	_FixedStack1 = _FixedStack0 - 1
	// 2047
	_FixedStack2 = _FixedStack1 | (_FixedStack1 >> 1)
	// 2047
	_FixedStack3 = _FixedStack2 | (_FixedStack2 >> 2)
	// 2047
	_FixedStack4 = _FixedStack3 | (_FixedStack3 >> 4)
	// 2047
	_FixedStack5 = _FixedStack4 | (_FixedStack4 >> 8)
	// 2047
	_FixedStack6 = _FixedStack5 | (_FixedStack5 >> 16)
	_FixedStack  = _FixedStack6 + 1 // 2048

	// Functions that need frames bigger than this use an extra
	// instruction to do the stack split check, to avoid overflow
	// in case SP - framesize wraps below zero.
	// This value can be no bigger than the size of the unmapped
	// space at zero.
	//
	// 第二种栈增长形式的检测，栈大于128小于等于4096字节。
	// SP - (framesize - _StackSmall <= gp.stackguard0 {
	//  	goto morestack
	// }
	// 减去 _StackSmall 是因为 stackguard0 以下128字节是可以安全使用的。
	//
	// 第三种栈增长形式的检测，栈大小大于4096
	// SP + _StackGuard - gp.stackguard0 < = framesize + _StackGuard - _StackSmall
	// 两边都加 _StackGuard 是为了防止 SP < gp.stackguard0 时无符号减法地正数的情况。
	// SP最多之比gp.stackGuard0小于128，而 _StackGuard 是928，左右两边都是正值。
	_StackBig = 4096

	// The stack guard is a pointer this many bytes above the
	// bottom of the stack.
	//
	// The guard leaves enough room for one _StackSmall frame plus
	// a _StackLimit chain of NOSPLIT calls plus _StackSystem
	// bytes for the OS.
	// This arithmetic must match that in cmd/internal/objabi/stack.go:StackLimit.
	_StackGuard = 928*sys.StackGuardMultiplier + _StackSystem

	// After a stack split check the SP is allowed to be this
	// many bytes below the stack guard. This saves an instruction
	// in the checking sequence for tiny frames.
	//
	// 第一种形式的栈增长检测形式针对函数栈帧大小不超过 _StackSmall 时，属于较小
	// 栈帧的情况。只有栈指针SP的位置没有超过 stackguard0 的界限，就不用进行栈
	// 增长。
	// SP <= gp.stackguard0 时跳转到栈增长。
	_StackSmall = 128

	// The maximum number of bytes that a chain of NOSPLIT
	// functions can use.
	// This arithmetic must match that in cmd/internal/objabi/stack.go:StackLimit.
	_StackLimit = _StackGuard - _StackSystem - _StackSmall
)

const (
	// stackDebug == 0: no logging
	//            == 1: logging of per-stack operations
	//            == 2: logging of per-frame operations
	//            == 3: logging of per-word updates
	//            == 4: logging of per-word reads
	stackDebug       = 0
	stackFromSystem  = 0 // allocate stacks from system memory instead of the heap
	stackFaultOnFree = 0 // old stacks are mapped noaccess to detect use after free
	stackPoisonCopy  = 0 // fill stack that should not be accessed with garbage, to detect bad dereferences during copy
	// runtime 构建时关闭了 stackcache。
	stackNoCache     = 0 // disable per-P small stack caches

	// check the BP links during traceback.
	debugCheckBP = false
)

const (
	uintptrMask = 1<<(8*goarch.PtrSize) - 1

	// The values below can be stored to g.stackguard0 to force
	// the next stack check to fail.
	// These are all larger than any real SP.

	// Goroutine preemption request.
	// 0xfffffade in hex.
	stackPreempt = uintptrMask & -1314

	// Thread is forking. Causes a split stack check failure.
	// 0xfffffb2e in hex.
	stackFork = uintptrMask & -1234

	// Force a stack movement. Used for debugging.
	// 0xfffffeed in hex.
	stackForceMove = uintptrMask & -275

	// stackPoisonMin is the lowest allowed stack poison value.
	stackPoisonMin = uintptrMask & -4096
)

// Global pool of spans that have free stacks.
// Stacks are assigned an order according to size.
//
//	order = log_2(size/FixedStack)
//
// There is a free list for each order.
//
// _NumStackOrders 在Linux平台下位4.
// 4个数组元素分别用来分配大小为 2KB、4KB、8KB和16KB的栈，更大的栈空间由 stackLarge 来分配。
// 其链表中的 mspan 通过 mheap.allocManual 从堆中直接分配的一个32KB大小的 mspan。
// 如果 mspan 用完了，通过 mheap.freeManual 函数回收。
var stackpool [_NumStackOrders]struct {
	// item.span 虽然是个链表，但是每次分配时(为空时，或者链表中的mspan已经用完了)，只会从堆
	// 上请求一个 32KB的 mspan，没有空闲obj的msapn会被回收。所以正常情况下这个链表只会有一个
	// mspan。
	// 但当 stackpoolfree 函数被调用时，它会在头部插入一些没有空闲obj的mspan到链表中，然后更新那个
	// msapn.manualFreeList = 回收栈的起始地址。也就是这样被插入的 msapn 只有一个空闲obj可
	// 用。且整个回收的mspan不一定是32kB。
	item stackpoolItem
	// 用于内存对齐的填充空间。
	// 填充空间的作用是把整个结构体的大小对齐到平台 Cache Line 的大小，以便最大限度地优化存取速度。
	_    [(cpu.CacheLinePadSize - unsafe.Sizeof(stackpoolItem{})%cpu.CacheLinePadSize) % cpu.CacheLinePadSize]byte
}

type stackpoolItem struct {
	_    sys.NotInHeap
	mu   mutex
	// mSpanList 是一个由 mspan 构成的双向链表， mutex 用来保护整个链表，真正的栈内存由链表中的 mspan 来提供。
	// span 链表中的 mspan 并不是obj为2KB、4KB、8KB和16KB的mspan，而是大小都为32KB的 mspan,然后使用
	// mspan.manualFreeList 链接。 gclinkptr 可以转换为 gclink 类型，其有一个 gclink.next 字段用于链接下个一个
	// 内存块。
	//
	// 如果链表 span 中不只一个元素的话，只有最后一个 mspan 是手动从堆分配的32KB大小。其他都是通过
	// stackpoolfree 函数回收的，且只有一个空闲obj被 mspan.manualFreeList 引用。
	span mSpanList
}

// Global pool of large stack spans.
//
// 在申请栈空间时如果在 stackLarge 对应链表
// 如果为空，不会像从 stackpool 那样填充后再分配，
// 而是直接从堆中申请连续的页构成整个mspan。
//
// 在栈收缩时或者GC markrootFreeGStacks() ，
// 回收的栈内存如果大于16KB会被填充到 stackLarge，
// 这是填充 stackLarge 的唯一方方式。
//
// mSpanList 中的每个mspan的 nelems = 1，用整个mspan表示一个栈。
// 不会像 stackpool 分割成内存块。
var stackLarge struct {
	// 由于在实际运行中对大于16KB的栈需求较少，所以这些针对不同大小的链表共用一把锁
	// 就可以了。
	// 像 stackpoll 则不然，因为使用频率较高，所以要为每个链表分配一个锁。
	lock mutex
	// linux 平台下 heapAddrBits 是48 , pageShit 是13，所以
	// free字段就是长度为25的 mSpanList 数组。
	// 下标为0的链表对应_PageSize，用来分配8KB的空间，后续依次翻倍。
	// 最大支持 2^27KB大小mspan 链表。
	//
	// 链表中的都是只有一个obj的mspan，在分配stack时不像其他几种栈缓存
	// 链表，在链表为空时会进行填充，会先填充再分配。当需要大于等于32KB的
	// 大小的栈时直接从堆中分配一个mspan返回。
	//
	// 其链表的填充来自于 stacfree 函数调用。
	free [heapAddrBits - pageShift]mSpanList // free lists by log_2(s.npages)
}
// stackinit() 函数被 schedinit() 函数调用。
// 此函数会初始化两个用于栈分配的全局对象，一个是栈缓冲池 stackpool ，另一个是专门用来分配大栈
// 的 stackLarge 。
func stackinit() {
	// 校验了 _StackCacheSzie 必须被定义为 _PageSize 的整数倍，并把 stackpool 和 stackLarge
	// 中的链表都初始化为空链表。
	// _StackCacheSize 目前大小为 32768
	if _StackCacheSize&_PageMask != 0 {
		throw("cache size must be a multiple of page size")
	}
	// 小于32KB的栽缓存池。
	// 数组stackpoll的大小4
	for i := range stackpool {
		// 用nil初始化初始双向链表的first和last字段。
		stackpool[i].item.span.init()
		// 为每个 stackpoolItem 初始化锁。
		lockInit(&stackpool[i].item.mu, lockRankStackpool)
	}
	// 大于等于32KB的栈缓冲池。
	for i := range stackLarge.free {
		// 用nil初始化初始双向链表的first和last字段。
		stackLarge.free[i].init()
		// stackLarge 不同大小的栈链表公用一把锁。
		// 因为大于16KB的栈比较少用。
		lockInit(&stackLarge.lock, lockRankStackLarge)
	}
}

// stacklog2 returns ⌊log_2(n)⌋.
func stacklog2(n uintptr) int {
	log2 := 0
	for n > 1 {
		n >>= 1
		log2++
	}
	return log2
}

// Allocates a stack from the free pool. Must be called with
// stackpool[order].item.mu held.
//
// 先尝试从 stackpool 中取得与目标大小对应的链表，如果链表为空，就从堆上分配一个
// 大小等于 _StackCacheSize 的maspan，手动将其划分成目标大小的内存块，添加到
// manualFreeList中，然后把新的 mspan 添加到 stackpool 对应的链表中。
// 最终栈是从 mspan 中分配的，实际上就是从 manualFreeList 中取出一个内存块，
// 并增加 allocCount 计数，如果 mspan 已经没有空间了，就把它从 stackpool 中
// 移除。
func stackpoolalloc(order uint8) gclinkptr {
	list := &stackpool[order].item.span
	s := list.first
	lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)
	if s == nil {
		// no free stacks. Allocate another span worth.
		//
		// 分配 32KB>>13 4页大小的mspan
		s = mheap_.allocManual(_StackCacheSize>>_PageShift, spanAllocStack)
		if s == nil {
			throw("out of memory")
		}
		if s.allocCount != 0 {
			throw("bad allocCount")
		}
		if s.manualFreeList.ptr() != nil {
			throw("bad manualFreeList")
		}
		// openBsd 平台需要，其它平台此函数为空。
		osStackAlloc(s)
		// 2KB << order，就是要申请的栈空间大小。
		// mspan 按此分割。
		s.elemsize = _FixedStack << order
		for i := uintptr(0); i < _StackCacheSize; i += s.elemsize {
			// x为每个obj起始地址，用每个内存块的第一个字节存储下一个obj的起始地址将
			// 它们连接起来。
			// 从地址高的内存块链向地址地的内存块。
			// s.manualFreeList 执行最后一个内存块的起始地址。
			x := gclinkptr(s.base() + i)
			// x.prt().next 设置为上一个内存的起始地址。
			x.ptr().next = s.manualFreeList
			s.manualFreeList = x
		}
		// 将span插入对应的链表list的头部。
		list.insert(s)
	}
	x := s.manualFreeList
	if x.ptr() == nil {
		throw("span has no free stacks")
	}
	// 更新 s.manualFreeList，因为我们马上要取出第一个元素。
	s.manualFreeList = x.ptr().next
	s.allocCount++
	if s.manualFreeList.ptr() == nil {
		// all stacks in s are allocated.
		// s(mspan)中所有的obj都使用了，可以将其从链表中移除了。
		list.remove(s)
	}
	return x
}

// Adds stack x to the free pool. Must be called with stackpool[order].item.mu held.
// stackpoolfree()
// 目的：
// 1. 将x所在obj从其所在 mspan 中的计数 mspan.allocCount 中减去。
// 更新 mspan.manualFreeList = x; x.ptr().next = mspan.manualFreeList
// ---可能会做的---
// 2. 如果检测到 mspan.manualFreeList=nil，将其插入到 stackpool 相应的order列表。
// 3. 如果当前处于 _GCoff 阶段且 mspan.allocCount = 0，将其所占的页回收到堆中。
//
// 参数：x: 要回收栈的起始地址，order: 应该被放入数组元素链表时的元素索引。
// stackpoolfree()函数目前只在系统栈上被调用。
// 回收的只是一个mspan中的一个obj，其它obj的状态未知。
// 所以我们只能处理这个obj。
func stackpoolfree(x gclinkptr, order uint8) {
	// 计算x地址归属的 mspan。
	s := spanOfUnchecked(uintptr(x))
	// 用于给栈分配内存的 mspan state必是 mSpanManual
	if s.state.get() != mSpanManual {
		throw("freeing stack not in a stack span")
	}
	if s.manualFreeList.ptr() == nil {
		// s的所有obj都被分配完了，可以将其安全地插入到span。
		// s will now have a free stack
		stackpool[order].item.span.insert(s)
	}
	// 因为接下来我们会将x表示的栈重用所以需要更新 s.manualFreeList = x
	// 以便下次分配stack的时候直接使用。
	x.ptr().next = s.manualFreeList
	s.manualFreeList = x
	// 分配计数减一
	s.allocCount--
	if gcphase == _GCoff && s.allocCount == 0 {
		// gcphase必须处于_GCoff 阶段，与栈关联的sudog扫描相关。
		//
		// 这里检测是gcphase为_GCoff，在此函数返回之前肯定都是_GCoff，
		// 因为此函数目前都是在系统栈中执行了，所以不会发生抢占已开启GC。
		//
		// 如果x所在obj是最后一个被清除的obj，那么在清除整个obj之后
		// 整个 mspan 便没有再被任何栈使用了，可以归还给堆了。
		//
		// Span is completely free. Return it to the heap
		// immediately if we're sweeping.
		//
		// If GC is active, we delay the free until the end of
		// GC to avoid the following type of situation:
		//
		// 1) GC starts, scans a SudoG but does not yet mark the SudoG.elem pointer
		// 2) The stack that pointer points to is copied
		// 3) The old stack is freed
		// 4) The containing span is marked free
		// 5) GC attempts to mark the SudoG.elem pointer. The
		//    marking fails because the pointer looks like a
		//    pointer into a free span.
		//
		// By not freeing, we prevent step #4 until GC is done.
		stackpool[order].item.span.remove(s)
		s.manualFreeList = 0
		osStackFree(s)
		mheap_.freeManual(s, spanAllocStack)
	}
}

// stackcacherefill()，目前只被 stackalloc 函数所调用。
// 目的：
// 用于初始化p stack cache。
// 给 mcache.stackcache[order] 赋值的链表总大小为16KB。
//
// 参数：
// c: 链表赋值的目标所在的 mcache。
// order: 定位到list对应的栈链表种类。
//
// stackcacherefill/stackcacherelease implement a global pool of stack segments.
// The pool is required to prevent unlimited growth of per-thread caches.
//
//go:systemstack
func stackcacherefill(c *mcache, order uint8) {
	if stackDebug >= 1 {
		print("stackcacherefill order=", order, "\n")
	}

	// Grab some stacks from the global cache.
	// Grab half of the allowed capacity (to prevent thrashing).
	var list gclinkptr
	var size uintptr
	lock(&stackpool[order].item.mu)
	for size < _StackCacheSize/2 {
		x := stackpoolalloc(order)
		x.ptr().next = list
		list = x
		size += _FixedStack << order
	}
	unlock(&stackpool[order].item.mu)
	c.stackcache[order].list = list
	c.stackcache[order].size = size
}

// stackcacherelease()函数目前只在 stackfree()函数中被调用。
//
// 链表中全部栈的大小不能超过16KB，多余的栈释放到 stackpool 中。
//go:systemstack
func stackcacherelease(c *mcache, order uint8) {
	if stackDebug >= 1 {
		print("stackcacherelease order=", order, "\n")
	}
	x := c.stackcache[order].list
	size := c.stackcache[order].size
	lock(&stackpool[order].item.mu)
	for size > _StackCacheSize/2 {
		y := x.ptr().next
		stackpoolfree(x, order)
		x = y
		size -= _FixedStack << order
	}
	unlock(&stackpool[order].item.mu)
	c.stackcache[order].list = x
	c.stackcache[order].size = size
}

//go:systemstack
func stackcache_clear(c *mcache) {
	if stackDebug >= 1 {
		print("stackcache clear\n")
	}
	for order := uint8(0); order < _NumStackOrders; order++ {
		lock(&stackpool[order].item.mu)
		x := c.stackcache[order].list
		for x.ptr() != nil {
			y := x.ptr().next
			stackpoolfree(x, order)
			x = y
		}
		c.stackcache[order].list = 0
		c.stackcache[order].size = 0
		unlock(&stackpool[order].item.mu)
	}
}

// stackalloc allocates an n byte stack.
//
// stackalloc must run on the system stack because it uses per-P
// resources and must not split the stack.
//
// stackalloc()函数在 gfget 、 malg 和 copystack 中被调用。
//
// 参数：
// n: 表示要分配的栈空间大小，它必须是2的幂。返回的 stack 结构用来表示
// 分配的栈空间， hi 字段是告地状，也就是栈空间的上界，lo表示空间下界。
//
//go:systemstack
func stackalloc(n uint32) stack {
	// Stackalloc must be called on scheduler stack, so that we
	// never try to grow the stack during the code that stackalloc runs.
	// Doing so would cause a deadlock (issue 1547).
	thisg := getg()
	if thisg != thisg.m.g0 {
		throw("stackalloc not on scheduler stack")
	}
	// n必须是2的整数幂运算后的结果。
	if n&(n-1) != 0 {
		throw("stack size not a power of 2")
	}
	if stackDebug >= 1 {
		print("stackalloc ", n, "\n")
	}

	if debug.efence != 0 || stackFromSystem != 0 {
		n = uint32(alignUp(uintptr(n), physPageSize))
		v := sysAlloc(uintptr(n), &memstats.stacks_sys)
		if v == nil {
			throw("out of memory (stackalloc)")
		}
		return stack{uintptr(v), uintptr(v) + uintptr(n)}
	}

	// Small stacks are allocated with a fixed-size free-list allocator.
	// If we need a stack of a bigger size, we fall back on allocating
	// a dedicated span.
	//
	// v 存储的是分配栈的起始地址
	var v unsafe.Pointer
	if n < _FixedStack<<_NumStackOrders && n < _StackCacheSize {
		// n < (2048<<4=32KB) && n < 32KB
		// order 为数组的索引，对应的元素是包含n大小栈的链表。
		order := uint8(0)
		n2 := n
		// order = log2(n2/_FixedStack)
		// 2kB order=0
		// 4KB order=1
		// 8KB order=2
		// 16KB order=3
		for n2 > _FixedStack {
			order++
			n2 >>= 1
		}

		// 分配栈的起始地址。
		var x gclinkptr
		// 下面几种情况不允许使用 p.stackcache，转而从stackpool中分配，会产生并发问题：
		// 1. runtime 构建时关闭了 stackNoCache。
		// 2. this.m.p 没有p自然无法使用stachCache。
		// 3. GC正在运行时。
		if stackNoCache != 0 || thisg.m.p == 0 || thisg.m.preemptoff != "" {
			// thisg.m.p == 0 can happen in the guts of exitsyscall
			// or procresize. Just get a stack from the global pool.
			// Also don't touch stackcache during gc
			// as it's flushed concurrently.
			lock(&stackpool[order].item.mu)
			// 从全局 stackpool 中分配。
			x = stackpoolalloc(order)
			unlock(&stackpool[order].item.mu)
		} else {
			// 从 p.mcache.stackcace 中分配。
			c := thisg.m.p.ptr().mcache
			x = c.stackcache[order].list
			if x.ptr() == nil {
				// 第一次就会是nil。
				//
				// 如果本地缓存为空，从全局 stackpool 分配 (16KB/2048<<order)个栈，然后将其链接
				// 起来赋值给 c.stackcache[order].list。
				//
				// 不同order链表中包含的栈数量：
				// order=0，分配8个
				// order=1，分配4个
				// order=2,分配2个
				// order=3,分配1个
				stackcacherefill(c, order)
				// x为链表的首部
				x = c.stackcache[order].list
			}
			// 移动链表的起始位置。
			// 即x指向的内存块的第地址大小内存块上存储的值
			c.stackcache[order].list = x.ptr().next
			// 更新链表还剩多少字节。
			c.stackcache[order].size -= uintptr(n)
		}
		v = unsafe.Pointer(x)
	} else {
		// 栈大小大于等于32KB
		var s *mspan
		// _PageShift = 13
		npage := uintptr(n) >> _PageShift
		// npage 表示n的大小是 pageSize(8KB) 的几倍。
		log2npage := stacklog2(npage)
		// log2npage 表示 stackLarge 中n大小的内存块链表所在的索引。
		// 0-25 索引0处存储的8KB链表，索引log2npage处存储的是 2^log2npage * 8KB 大小内存块的链表。
		// Try to get a stack from the large stack cache.
		lock(&stackLarge.lock)
		if !stackLarge.free[log2npage].isEmpty() {
			// stackLarge 对应大小的栈链不为空。
			s = stackLarge.free[log2npage].first
			stackLarge.free[log2npage].remove(s)
		}
		unlock(&stackLarge.lock)

		// 空函数。
		lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)

		if s == nil {
			// 对应链表为空
			// Allocate a new stack from the heap.
			// 尝试直接从 arena 中分配。
			s = mheap_.allocManual(npage, spanAllocStack)
			if s == nil {
				throw("out of memory")
			}
			osStackAlloc(s)
			// 整个mspan表表示一个栈空间。
			s.elemsize = uintptr(n)
		}
		// v就是mspan的起始地址。
		v = unsafe.Pointer(s.base())
	}

	if raceenabled {
		racemalloc(v, uintptr(n))
	}
	if msanenabled {
		msanmalloc(v, uintptr(n))
	}
	if asanenabled {
		asanunpoison(v, uintptr(n))
	}
	if stackDebug >= 1 {
		print("  allocated ", v, "\n")
	}
	// 左和右开
	return stack{uintptr(v), uintptr(v) + uintptr(n)}
}

// stackfree frees an n byte stack allocation at stk.
// 只有权回收 stk 表示内存块，而不是其所在的整个 mspan。
//
// 根据栈大小和GC状态，分别回收到 mcache.stackcache 、 stackpool 和
// stackLarge 或者释放到堆。
// 参数：stk，包含了栈的起始和结束地址信息，左闭右开区间。
//
// stackfree must run on the system stack because it uses per-P
// resources and must not split the stack.
//
//go:systemstack
func stackfree(stk stack) {
	gp := getg()
	// v 要回收的内存块(obj)起始地址。
	v := unsafe.Pointer(stk.lo)
	// n 要回收的内存块(obj)的大小。
	n := stk.hi - stk.lo
	// 大小必须是2的整数幂的结果。
	if n&(n-1) != 0 {
		throw("stack not a power of 2")
	}
	if stk.lo+n < stk.hi {
		throw("bad stack size")
	}
	if stackDebug >= 1 {
		println("stackfree", v, n)
		memclrNoHeapPointers(v, n) // for testing, clobber stack data
	}
	if debug.efence != 0 || stackFromSystem != 0 {
		if debug.efence != 0 || stackFaultOnFree != 0 {
			sysFault(v, n)
		} else {
			sysFree(v, n, &memstats.stacks_sys)
		}
		return
	}
	if msanenabled {
		msanfree(v, n)
	}
	if asanenabled {
		asanpoison(v, n)
	}
	if n < _FixedStack<<_NumStackOrders && n < _StackCacheSize {
		// n< 2048<<4 && n<32KB = n<32KB
		// 这个大小的 stk 要么回收到 p.stackcache，要么回收到 stackpool中。
		order := uint8(0)
		n2 := n
		for n2 > _FixedStack {
			order++
			n2 >>= 1
		}
		// n2 = order
		x := gclinkptr(v)
		if stackNoCache != 0 || gp.m.p == 0 || gp.m.preemptoff != "" {
			// p.stackcache 不可用。回收到 stackpool。
			lock(&stackpool[order].item.mu)
			stackpoolfree(x, order)
			unlock(&stackpool[order].item.mu)
		} else {
			c := gp.m.p.ptr().mcache
			// 链表中的栈总大小大于等于32KB，则回收。
			if c.stackcache[order].size >= _StackCacheSize {
				stackcacherelease(c, order)
			}
			x.ptr().next = c.stackcache[order].list
			c.stackcache[order].list = x
			c.stackcache[order].size += n
		}
	} else {
		// 被回收的栈是大于或等于32KB，这样的栈所在的mspan都只是
		// 单obj的，所以栈的回收等于整个mspan的回收。
		s := spanOfUnchecked(uintptr(v))
		if s.state.get() != mSpanManual {
			println(hex(s.base()), v)
			throw("bad span state")
		}
		if gcphase == _GCoff {
			// 如果g关联了sudog，在GC标记阶段进行mspan的堆回收可能造成错误。
			// 参见: stackpoolfree()函数的注释。
			//
			// 可以安全的回收掉整个mspan。
			//
			// Free the stack immediately if we're
			// sweeping.
			osStackFree(s)
			mheap_.freeManual(s, spanAllocStack)
		} else {
			// If the GC is running, we can't return a
			// stack span to the heap because it could be
			// reused as a heap span, and this state
			// change would race with GC. Add it to the
			// large stack cache instead.
			//
			// 直接插入到链表中不用其它处理，因为整个mspan就是一个栈大小。
			// 这里也是唯一向 stackLarge.free 链表插入元素的地方。
			log2npage := stacklog2(s.npages)
			lock(&stackLarge.lock)
			// 插入表头。
			stackLarge.free[log2npage].insert(s)
			unlock(&stackLarge.lock)
		}
	}
}

var maxstacksize uintptr = 1 << 20 // enough until runtime.main sets it for real

var maxstackceiling = maxstacksize

var ptrnames = []string{
	0: "scalar",
	1: "ptr",
}

// Stack frame layout
//
// (x86)
// +------------------+
// | args from caller |
// +------------------+ <- frame->argp
// |  return address  |
// +------------------+
// |  caller's BP (*) | (*) if framepointer_enabled && varp < sp
// +------------------+ <- frame->varp
// |     locals       |
// +------------------+
// |  args to callee  |
// +------------------+ <- frame->sp
//
// (arm)
// +------------------+
// | args from caller |
// +------------------+ <- frame->argp
// | caller's retaddr |
// +------------------+ <- frame->varp
// |     locals       |
// +------------------+
// |  args to callee  |
// +------------------+
// |  return address  |
// +------------------+ <- frame->sp

type adjustinfo struct {
	old   stack
	delta uintptr // ptr distance from old to new stack (newbase - oldbase)
	cache pcvalueCache

	// sghi is the highest sudog.elem on the stack.
	sghi uintptr
}

// adjustpointer checks whether *vpp is in the old stack described by adjinfo.
// If so, it rewrites *vpp to point into the new stack.
func adjustpointer(adjinfo *adjustinfo, vpp unsafe.Pointer) {
	pp := (*uintptr)(vpp)
	p := *pp
	if stackDebug >= 4 {
		print("        ", pp, ":", hex(p), "\n")
	}
	if adjinfo.old.lo <= p && p < adjinfo.old.hi {
		*pp = p + adjinfo.delta
		if stackDebug >= 3 {
			print("        adjust ptr ", pp, ":", hex(p), " -> ", hex(*pp), "\n")
		}
	}
}

// Information from the compiler about the layout of stack frames.
// Note: this type must agree with reflect.bitVector.
type bitvector struct {
	n        int32 // # of bits
	bytedata *uint8
}

// ptrbit returns the i'th bit in bv.
// ptrbit is less efficient than iterating directly over bitvector bits,
// and should only be used in non-performance-critical code.
// See adjustpointers for an example of a high-efficiency walk of a bitvector.
func (bv *bitvector) ptrbit(i uintptr) uint8 {
	b := *(addb(bv.bytedata, i/8))
	return (b >> (i % 8)) & 1
}

// bv describes the memory starting at address scanp.
// Adjust any pointers contained therein.
func adjustpointers(scanp unsafe.Pointer, bv *bitvector, adjinfo *adjustinfo, f funcInfo) {
	minp := adjinfo.old.lo
	maxp := adjinfo.old.hi
	delta := adjinfo.delta
	num := uintptr(bv.n)
	// If this frame might contain channel receive slots, use CAS
	// to adjust pointers. If the slot hasn't been received into
	// yet, it may contain stack pointers and a concurrent send
	// could race with adjusting those pointers. (The sent value
	// itself can never contain stack pointers.)
	useCAS := uintptr(scanp) < adjinfo.sghi
	for i := uintptr(0); i < num; i += 8 {
		if stackDebug >= 4 {
			for j := uintptr(0); j < 8; j++ {
				print("        ", add(scanp, (i+j)*goarch.PtrSize), ":", ptrnames[bv.ptrbit(i+j)], ":", hex(*(*uintptr)(add(scanp, (i+j)*goarch.PtrSize))), " # ", i, " ", *addb(bv.bytedata, i/8), "\n")
			}
		}
		b := *(addb(bv.bytedata, i/8))
		for b != 0 {
			j := uintptr(sys.TrailingZeros8(b))
			b &= b - 1
			pp := (*uintptr)(add(scanp, (i+j)*goarch.PtrSize))
		retry:
			p := *pp
			if f.valid() && 0 < p && p < minLegalPointer && debug.invalidptr != 0 {
				// Looks like a junk value in a pointer slot.
				// Live analysis wrong?
				getg().m.traceback = 2
				print("runtime: bad pointer in frame ", funcname(f), " at ", pp, ": ", hex(p), "\n")
				throw("invalid pointer found on stack")
			}
			if minp <= p && p < maxp {
				if stackDebug >= 3 {
					print("adjust ptr ", hex(p), " ", funcname(f), "\n")
				}
				if useCAS {
					ppu := (*unsafe.Pointer)(unsafe.Pointer(pp))
					if !atomic.Casp1(ppu, unsafe.Pointer(p), unsafe.Pointer(p+delta)) {
						goto retry
					}
				} else {
					*pp = p + delta
				}
			}
		}
	}
}

// Note: the argument/return area is adjusted by the callee.
func adjustframe(frame *stkframe, arg unsafe.Pointer) bool {
	adjinfo := (*adjustinfo)(arg)
	if frame.continpc == 0 {
		// Frame is dead.
		return true
	}
	f := frame.fn
	if stackDebug >= 2 {
		print("    adjusting ", funcname(f), " frame=[", hex(frame.sp), ",", hex(frame.fp), "] pc=", hex(frame.pc), " continpc=", hex(frame.continpc), "\n")
	}
	if f.funcID == funcID_systemstack_switch {
		// A special routine at the bottom of stack of a goroutine that does a systemstack call.
		// We will allow it to be copied even though we don't
		// have full GC info for it (because it is written in asm).
		return true
	}

	locals, args, objs := frame.getStackMap(&adjinfo.cache, true)

	// Adjust local variables if stack frame has been allocated.
	if locals.n > 0 {
		size := uintptr(locals.n) * goarch.PtrSize
		adjustpointers(unsafe.Pointer(frame.varp-size), &locals, adjinfo, f)
	}

	// Adjust saved base pointer if there is one.
	// TODO what about arm64 frame pointer adjustment?
	if goarch.ArchFamily == goarch.AMD64 && frame.argp-frame.varp == 2*goarch.PtrSize {
		if stackDebug >= 3 {
			print("      saved bp\n")
		}
		if debugCheckBP {
			// Frame pointers should always point to the next higher frame on
			// the Go stack (or be nil, for the top frame on the stack).
			bp := *(*uintptr)(unsafe.Pointer(frame.varp))
			if bp != 0 && (bp < adjinfo.old.lo || bp >= adjinfo.old.hi) {
				println("runtime: found invalid frame pointer")
				print("bp=", hex(bp), " min=", hex(adjinfo.old.lo), " max=", hex(adjinfo.old.hi), "\n")
				throw("bad frame pointer")
			}
		}
		adjustpointer(adjinfo, unsafe.Pointer(frame.varp))
	}

	// Adjust arguments.
	if args.n > 0 {
		if stackDebug >= 3 {
			print("      args\n")
		}
		adjustpointers(unsafe.Pointer(frame.argp), &args, adjinfo, funcInfo{})
	}

	// Adjust pointers in all stack objects (whether they are live or not).
	// See comments in mgcmark.go:scanframeworker.
	if frame.varp != 0 {
		for i := range objs {
			obj := &objs[i]
			off := obj.off
			base := frame.varp // locals base pointer
			if off >= 0 {
				base = frame.argp // arguments and return values base pointer
			}
			p := base + uintptr(off)
			if p < frame.sp {
				// Object hasn't been allocated in the frame yet.
				// (Happens when the stack bounds check fails and
				// we call into morestack.)
				continue
			}
			ptrdata := obj.ptrdata()
			gcdata := obj.gcdata()
			var s *mspan
			if obj.useGCProg() {
				// See comments in mgcmark.go:scanstack
				s = materializeGCProg(ptrdata, gcdata)
				gcdata = (*byte)(unsafe.Pointer(s.startAddr))
			}
			for i := uintptr(0); i < ptrdata; i += goarch.PtrSize {
				if *addb(gcdata, i/(8*goarch.PtrSize))>>(i/goarch.PtrSize&7)&1 != 0 {
					adjustpointer(adjinfo, unsafe.Pointer(p+i))
				}
			}
			if s != nil {
				dematerializeGCProg(s)
			}
		}
	}

	return true
}

func adjustctxt(gp *g, adjinfo *adjustinfo) {
	adjustpointer(adjinfo, unsafe.Pointer(&gp.sched.ctxt))
	if !framepointer_enabled {
		return
	}
	if debugCheckBP {
		bp := gp.sched.bp
		if bp != 0 && (bp < adjinfo.old.lo || bp >= adjinfo.old.hi) {
			println("runtime: found invalid top frame pointer")
			print("bp=", hex(bp), " min=", hex(adjinfo.old.lo), " max=", hex(adjinfo.old.hi), "\n")
			throw("bad top frame pointer")
		}
	}
	adjustpointer(adjinfo, unsafe.Pointer(&gp.sched.bp))
}

func adjustdefers(gp *g, adjinfo *adjustinfo) {
	// Adjust pointers in the Defer structs.
	// We need to do this first because we need to adjust the
	// defer.link fields so we always work on the new stack.
	adjustpointer(adjinfo, unsafe.Pointer(&gp._defer))
	for d := gp._defer; d != nil; d = d.link {
		adjustpointer(adjinfo, unsafe.Pointer(&d.fn))
		adjustpointer(adjinfo, unsafe.Pointer(&d.sp))
		adjustpointer(adjinfo, unsafe.Pointer(&d._panic))
		adjustpointer(adjinfo, unsafe.Pointer(&d.link))
		adjustpointer(adjinfo, unsafe.Pointer(&d.varp))
		adjustpointer(adjinfo, unsafe.Pointer(&d.fd))
	}
}

func adjustpanics(gp *g, adjinfo *adjustinfo) {
	// Panics are on stack and already adjusted.
	// Update pointer to head of list in G.
	adjustpointer(adjinfo, unsafe.Pointer(&gp._panic))
}

func adjustsudogs(gp *g, adjinfo *adjustinfo) {
	// the data elements pointed to by a SudoG structure
	// might be in the stack.
	for s := gp.waiting; s != nil; s = s.waitlink {
		adjustpointer(adjinfo, unsafe.Pointer(&s.elem))
	}
}

func fillstack(stk stack, b byte) {
	for p := stk.lo; p < stk.hi; p++ {
		*(*byte)(unsafe.Pointer(p)) = b
	}
}

func findsghi(gp *g, stk stack) uintptr {
	var sghi uintptr
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		p := uintptr(sg.elem) + uintptr(sg.c.elemsize)
		if stk.lo <= p && p < stk.hi && p > sghi {
			sghi = p
		}
	}
	return sghi
}

// syncadjustsudogs adjusts gp's sudogs and copies the part of gp's
// stack they refer to while synchronizing with concurrent channel
// operations. It returns the number of bytes of stack copied.
func syncadjustsudogs(gp *g, used uintptr, adjinfo *adjustinfo) uintptr {
	if gp.waiting == nil {
		return 0
	}

	// Lock channels to prevent concurrent send/receive.
	var lastc *hchan
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc {
			// There is a ranking cycle here between gscan bit and
			// hchan locks. Normally, we only allow acquiring hchan
			// locks and then getting a gscan bit. In this case, we
			// already have the gscan bit. We allow acquiring hchan
			// locks here as a special case, since a deadlock can't
			// happen because the G involved must already be
			// suspended. So, we get a special hchan lock rank here
			// that is lower than gscan, but doesn't allow acquiring
			// any other locks other than hchan.
			lockWithRank(&sg.c.lock, lockRankHchanLeaf)
		}
		lastc = sg.c
	}

	// Adjust sudogs.
	adjustsudogs(gp, adjinfo)

	// Copy the part of the stack the sudogs point in to
	// while holding the lock to prevent races on
	// send/receive slots.
	var sgsize uintptr
	if adjinfo.sghi != 0 {
		oldBot := adjinfo.old.hi - used
		newBot := oldBot + adjinfo.delta
		sgsize = adjinfo.sghi - oldBot
		memmove(unsafe.Pointer(newBot), unsafe.Pointer(oldBot), sgsize)
	}

	// Unlock channels.
	lastc = nil
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc {
			unlock(&sg.c.lock)
		}
		lastc = sg.c
	}

	return sgsize
}

// Copies gp's stack to a new stack of a different size.
// Caller must have changed gp status to Gcopystack.
func copystack(gp *g, newsize uintptr) {
	if gp.syscallsp != 0 {
		throw("stack growth not allowed in system call")
	}
	old := gp.stack
	if old.lo == 0 {
		throw("nil stackbase")
	}
	used := old.hi - gp.sched.sp
	// Add just the difference to gcController.addScannableStack.
	// g0 stacks never move, so this will never account for them.
	// It's also fine if we have no P, addScannableStack can deal with
	// that case.
	gcController.addScannableStack(getg().m.p.ptr(), int64(newsize)-int64(old.hi-old.lo))

	// allocate new stack
	new := stackalloc(uint32(newsize))
	if stackPoisonCopy != 0 {
		fillstack(new, 0xfd)
	}
	if stackDebug >= 1 {
		print("copystack gp=", gp, " [", hex(old.lo), " ", hex(old.hi-used), " ", hex(old.hi), "]", " -> [", hex(new.lo), " ", hex(new.hi-used), " ", hex(new.hi), "]/", newsize, "\n")
	}

	// Compute adjustment.
	var adjinfo adjustinfo
	adjinfo.old = old
	adjinfo.delta = new.hi - old.hi

	// Adjust sudogs, synchronizing with channel ops if necessary.
	ncopy := used
	if !gp.activeStackChans {
		if newsize < old.hi-old.lo && gp.parkingOnChan.Load() {
			// It's not safe for someone to shrink this stack while we're actively
			// parking on a channel, but it is safe to grow since we do that
			// ourselves and explicitly don't want to synchronize with channels
			// since we could self-deadlock.
			throw("racy sudog adjustment due to parking on channel")
		}
		adjustsudogs(gp, &adjinfo)
	} else {
		// sudogs may be pointing in to the stack and gp has
		// released channel locks, so other goroutines could
		// be writing to gp's stack. Find the highest such
		// pointer so we can handle everything there and below
		// carefully. (This shouldn't be far from the bottom
		// of the stack, so there's little cost in handling
		// everything below it carefully.)
		adjinfo.sghi = findsghi(gp, old)

		// Synchronize with channel ops and copy the part of
		// the stack they may interact with.
		ncopy -= syncadjustsudogs(gp, used, &adjinfo)
	}

	// Copy the stack (or the rest of it) to the new location
	memmove(unsafe.Pointer(new.hi-ncopy), unsafe.Pointer(old.hi-ncopy), ncopy)

	// Adjust remaining structures that have pointers into stacks.
	// We have to do most of these before we traceback the new
	// stack because gentraceback uses them.
	adjustctxt(gp, &adjinfo)
	adjustdefers(gp, &adjinfo)
	adjustpanics(gp, &adjinfo)
	if adjinfo.sghi != 0 {
		adjinfo.sghi += adjinfo.delta
	}

	// Swap out old stack for new one
	gp.stack = new
	gp.stackguard0 = new.lo + _StackGuard // NOTE: might clobber a preempt request
	gp.sched.sp = new.hi - used
	gp.stktopsp += adjinfo.delta

	// Adjust pointers in the new stack.
	gentraceback(^uintptr(0), ^uintptr(0), 0, gp, 0, nil, 0x7fffffff, adjustframe, noescape(unsafe.Pointer(&adjinfo)), 0)

	// free old stack
	if stackPoisonCopy != 0 {
		fillstack(old, 0xfc)
	}
	stackfree(old)
}

// round x up to a power of 2.
// 向上对齐到2^n
func round2(x int32) int32 {
	s := uint(0)
	for 1<<s < x {
		s++
	}
	return 1 << s
}

// Called from runtime·morestack when more stack is needed.
// Allocate larger stack and relocate to new stack.
// Stack growth is multiplicative, for constant amortized cost.
//
// g->atomicstatus will be Grunning or Gscanrunning upon entry.
// If the scheduler is trying to stop this g, then it will set preemptStop.
//
// This must be nowritebarrierrec because it can be called as part of
// stack growth from other nowritebarrierrec functions, but the
// compiler doesn't check this.
//
//go:nowritebarrierrec
func newstack() {
	thisg := getg()
	// TODO: double check all gp. shouldn't be getg().
	if thisg.m.morebuf.g.ptr().stackguard0 == stackFork {
		throw("stack growth after fork")
	}
	if thisg.m.morebuf.g.ptr() != thisg.m.curg {
		print("runtime: newstack called from g=", hex(thisg.m.morebuf.g), "\n"+"\tm=", thisg.m, " m->curg=", thisg.m.curg, " m->g0=", thisg.m.g0, " m->gsignal=", thisg.m.gsignal, "\n")
		morebuf := thisg.m.morebuf
		traceback(morebuf.pc, morebuf.sp, morebuf.lr, morebuf.g.ptr())
		throw("runtime: wrong goroutine in newstack")
	}

	gp := thisg.m.curg

	if thisg.m.curg.throwsplit {
		// Update syscallsp, syscallpc in case traceback uses them.
		morebuf := thisg.m.morebuf
		gp.syscallsp = morebuf.sp
		gp.syscallpc = morebuf.pc
		pcname, pcoff := "(unknown)", uintptr(0)
		f := findfunc(gp.sched.pc)
		if f.valid() {
			pcname = funcname(f)
			pcoff = gp.sched.pc - f.entry()
		}
		print("runtime: newstack at ", pcname, "+", hex(pcoff),
			" sp=", hex(gp.sched.sp), " stack=[", hex(gp.stack.lo), ", ", hex(gp.stack.hi), "]\n",
			"\tmorebuf={pc:", hex(morebuf.pc), " sp:", hex(morebuf.sp), " lr:", hex(morebuf.lr), "}\n",
			"\tsched={pc:", hex(gp.sched.pc), " sp:", hex(gp.sched.sp), " lr:", hex(gp.sched.lr), " ctxt:", gp.sched.ctxt, "}\n")

		thisg.m.traceback = 2 // Include runtime frames
		traceback(morebuf.pc, morebuf.sp, morebuf.lr, gp)
		throw("runtime: stack split at bad time")
	}

	morebuf := thisg.m.morebuf
	thisg.m.morebuf.pc = 0
	thisg.m.morebuf.lr = 0
	thisg.m.morebuf.sp = 0
	thisg.m.morebuf.g = 0

	// NOTE: stackguard0 may change underfoot, if another thread
	// is about to try to preempt gp. Read it just once and use that same
	// value now and below.
	stackguard0 := atomic.Loaduintptr(&gp.stackguard0)

	// Be conservative about where we preempt.
	// We are interested in preempting user Go code, not runtime code.
	// If we're holding locks, mallocing, or preemption is disabled, don't
	// preempt.
	// This check is very early in newstack so that even the status change
	// from Grunning to Gwaiting and back doesn't happen in this case.
	// That status change by itself can be viewed as a small preemption,
	// because the GC might change Gwaiting to Gscanwaiting, and then
	// this goroutine has to wait for the GC to finish before continuing.
	// If the GC is in some way dependent on this goroutine (for example,
	// it needs a lock held by the goroutine), that small preemption turns
	// into a real deadlock.
	preempt := stackguard0 == stackPreempt
	if preempt {
		if !canPreemptM(thisg.m) {
			// Let the goroutine keep running for now.
			// gp->preempt is set, so it will be preempted next time.
			gp.stackguard0 = gp.stack.lo + _StackGuard
			gogo(&gp.sched) // never return
		}
	}

	if gp.stack.lo == 0 {
		throw("missing stack in newstack")
	}
	sp := gp.sched.sp
	if goarch.ArchFamily == goarch.AMD64 || goarch.ArchFamily == goarch.I386 || goarch.ArchFamily == goarch.WASM {
		// The call to morestack cost a word.
		sp -= goarch.PtrSize
	}
	if stackDebug >= 1 || sp < gp.stack.lo {
		print("runtime: newstack sp=", hex(sp), " stack=[", hex(gp.stack.lo), ", ", hex(gp.stack.hi), "]\n",
			"\tmorebuf={pc:", hex(morebuf.pc), " sp:", hex(morebuf.sp), " lr:", hex(morebuf.lr), "}\n",
			"\tsched={pc:", hex(gp.sched.pc), " sp:", hex(gp.sched.sp), " lr:", hex(gp.sched.lr), " ctxt:", gp.sched.ctxt, "}\n")
	}
	if sp < gp.stack.lo {
		print("runtime: gp=", gp, ", goid=", gp.goid, ", gp->status=", hex(readgstatus(gp)), "\n ")
		print("runtime: split stack overflow: ", hex(sp), " < ", hex(gp.stack.lo), "\n")
		throw("runtime: split stack overflow")
	}

	if preempt {
		if gp == thisg.m.g0 {
			throw("runtime: preempt g0")
		}
		if thisg.m.p == 0 && thisg.m.locks == 0 {
			throw("runtime: g is running but p is not")
		}

		if gp.preemptShrink {
			// We're at a synchronous safe point now, so
			// do the pending stack shrink.
			gp.preemptShrink = false
			shrinkstack(gp)
		}

		if gp.preemptStop {
			preemptPark(gp) // never returns
		}

		// Act like goroutine called runtime.Gosched.
		gopreempt_m(gp) // never return
	}

	// Allocate a bigger segment and move the stack.
	oldsize := gp.stack.hi - gp.stack.lo
	newsize := oldsize * 2

	// Make sure we grow at least as much as needed to fit the new frame.
	// (This is just an optimization - the caller of morestack will
	// recheck the bounds on return.)
	if f := findfunc(gp.sched.pc); f.valid() {
		max := uintptr(funcMaxSPDelta(f))
		needed := max + _StackGuard
		used := gp.stack.hi - gp.sched.sp
		for newsize-used < needed {
			newsize *= 2
		}
	}

	if stackguard0 == stackForceMove {
		// Forced stack movement used for debugging.
		// Don't double the stack (or we may quickly run out
		// if this is done repeatedly).
		newsize = oldsize
	}

	if newsize > maxstacksize || newsize > maxstackceiling {
		if maxstacksize < maxstackceiling {
			print("runtime: goroutine stack exceeds ", maxstacksize, "-byte limit\n")
		} else {
			print("runtime: goroutine stack exceeds ", maxstackceiling, "-byte limit\n")
		}
		print("runtime: sp=", hex(sp), " stack=[", hex(gp.stack.lo), ", ", hex(gp.stack.hi), "]\n")
		throw("stack overflow")
	}

	// The goroutine must be executing in order to call newstack,
	// so it must be Grunning (or Gscanrunning).
	casgstatus(gp, _Grunning, _Gcopystack)

	// The concurrent GC will not scan the stack while we are doing the copy since
	// the gp is in a Gcopystack status.
	copystack(gp, newsize)
	if stackDebug >= 1 {
		print("stack grow done\n")
	}
	casgstatus(gp, _Gcopystack, _Grunning)
	gogo(&gp.sched)
}

//go:nosplit
func nilfunc() {
	*(*uint8)(nil) = 0
}

// 参数：fv：闭包指针
// fn 函数指针
// adjust Gobuf as if it executed a call to fn
// and then stopped before the first instruction in fn.
func gostartcallfn(gobuf *gobuf, fv *funcval) {
	var fn unsafe.Pointer
	if fv != nil {
		fn = unsafe.Pointer(fv.fn)
	} else {
		fn = unsafe.Pointer(abi.FuncPCABIInternal(nilfunc))
	}
	gostartcall(gobuf, fn, unsafe.Pointer(fv))
}

// isShrinkStackSafe returns whether it's safe to attempt to shrink
// gp's stack. Shrinking the stack is only safe when we have precise
// pointer maps for all frames on the stack.
func isShrinkStackSafe(gp *g) bool {
	// We can't copy the stack if we're in a syscall.
	// The syscall might have pointers into the stack and
	// often we don't have precise pointer maps for the innermost
	// frames.
	//
	// We also can't copy the stack if we're at an asynchronous
	// safe-point because we don't have precise pointer maps for
	// all frames.
	//
	// We also can't *shrink* the stack in the window between the
	// goroutine calling gopark to park on a channel and
	// gp.activeStackChans being set.
	//
	// gp.activeStackChans表示gp是否因通道操作处于挂起状态。
	// 当gp.activeStackChans为true时，执行newStack时需先获取gp排队所在通道的锁。避免
	// 在栈收缩时修改关联sudog.elem与其它会唤醒gp的通道操作发生冲突。
	//
	// 当gp.parkingOnChan为false且gp.activeStackChans=true（activeStackChans是非原子性设置，
	// g暂停之前所在线程观察到的），则一定能观察到 gp.activeStackChans=true。从
	// 而保证在执行newstack时该上锁的chan一定会被上锁。防止此sudog被从chan等待队列中取出
	// 读取了上面存储的gp栈指针。
	return gp.syscallsp == 0 && !gp.asyncSafePoint && !gp.parkingOnChan.Load()
}

// Maybe shrink the stack being used by gp.
//
// gp must be stopped and we must own its stack. It may be in
// _Grunning, but only if this is our own user G.
func shrinkstack(gp *g) {
	if gp.stack.lo == 0 {
		throw("missing stack in shrinkstack")
	}
	if s := readgstatus(gp); s&_Gscan == 0 {
		// We don't own the stack via _Gscan. We could still
		// own it if this is our own user G and we're on the
		// system stack.
		if !(gp == getg().m.curg && getg() != getg().m.curg && s == _Grunning) {
			// We don't own the stack.
			throw("bad status in shrinkstack")
		}
	}
	if !isShrinkStackSafe(gp) {
		throw("shrinkstack at bad time")
	}
	// Check for self-shrinks while in a libcall. These may have
	// pointers into the stack disguised as uintptrs, but these
	// code paths should all be nosplit.
	if gp == getg().m.curg && gp.m.libcallsp != 0 {
		throw("shrinking stack in libcall")
	}

	if debug.gcshrinkstackoff > 0 {
		return
	}
	f := findfunc(gp.startpc)
	if f.valid() && f.funcID == funcID_gcBgMarkWorker {
		// We're not allowed to shrink the gcBgMarkWorker
		// stack (see gcBgMarkWorker for explanation).
		return
	}

	oldsize := gp.stack.hi - gp.stack.lo
	newsize := oldsize / 2
	// Don't shrink the allocation below the minimum-sized stack
	// allocation.
	if newsize < _FixedStack {
		return
	}
	// Compute how much of the stack is currently in use and only
	// shrink the stack if gp is using less than a quarter of its
	// current stack. The currently used stack includes everything
	// down to the SP plus the stack guard space that ensures
	// there's room for nosplit functions.
	avail := gp.stack.hi - gp.stack.lo
	if used := gp.stack.hi - gp.sched.sp + _StackLimit; used >= avail/4 {
		return
	}

	if stackDebug > 0 {
		print("shrinking stack ", oldsize, "->", newsize, "\n")
	}

	copystack(gp, newsize)
}

// freeStackSpans frees unused stack spans at the end of GC.
func freeStackSpans() {
	// Scan stack pools for empty stack spans.
	for order := range stackpool {
		lock(&stackpool[order].item.mu)
		list := &stackpool[order].item.span
		for s := list.first; s != nil; {
			next := s.next
			if s.allocCount == 0 {
				list.remove(s)
				s.manualFreeList = 0
				osStackFree(s)
				mheap_.freeManual(s, spanAllocStack)
			}
			s = next
		}
		unlock(&stackpool[order].item.mu)
	}

	// Free large stack spans.
	lock(&stackLarge.lock)
	for i := range stackLarge.free {
		for s := stackLarge.free[i].first; s != nil; {
			next := s.next
			stackLarge.free[i].remove(s)
			osStackFree(s)
			mheap_.freeManual(s, spanAllocStack)
			s = next
		}
	}
	unlock(&stackLarge.lock)
}

// A stackObjectRecord is generated by the compiler for each stack object in a stack frame.
// This record must match the generator code in cmd/compile/internal/liveness/plive.go:emitStackObjects.
type stackObjectRecord struct {
	// offset in frame
	// if negative, offset from varp
	// if non-negative, offset from argp
	off       int32
	size      int32
	_ptrdata  int32  // ptrdata, or -ptrdata is GC prog is used
	gcdataoff uint32 // offset to gcdata from moduledata.rodata
}

func (r *stackObjectRecord) useGCProg() bool {
	return r._ptrdata < 0
}

func (r *stackObjectRecord) ptrdata() uintptr {
	x := r._ptrdata
	if x < 0 {
		return uintptr(-x)
	}
	return uintptr(x)
}

// gcdata returns pointer map or GC prog of the type.
func (r *stackObjectRecord) gcdata() *byte {
	ptr := uintptr(unsafe.Pointer(r))
	var mod *moduledata
	for datap := &firstmoduledata; datap != nil; datap = datap.next {
		if datap.gofunc <= ptr && ptr < datap.end {
			mod = datap
			break
		}
	}
	// If you get a panic here due to a nil mod,
	// you may have made a copy of a stackObjectRecord.
	// You must use the original pointer.
	res := mod.rodata + uintptr(r.gcdataoff)
	return (*byte)(unsafe.Pointer(res))
}

// This is exported as ABI0 via linkname so obj can call it.
//
//go:nosplit
//go:linkname morestackc
func morestackc() {
	throw("attempt to execute system stack code on user stack")
}
// 创建g时，为其分配的栈大小。
//
// 初始值为：_FixedStack=2048
// 这个值在每次GC标记终止阶段会被更新。
// 本轮GC标记阶段被扫描栈区间(hi-sp)总和/被扫描栈的数量，
// 修正到 [ _FixedStack, maxstacksize]这个范围，然后向上对齐到2的幂。
//
// startingStackSize is the amount of stack that new goroutines start with.
// It is a power of 2, and between _FixedStack and maxstacksize, inclusive.
// startingStackSize is updated every GC by tracking the average size of
// stacks scanned during the GC.
var startingStackSize uint32 = _FixedStack

func gcComputeStartingStackSize() {
	if debug.adaptivestackstart == 0 {
		return
	}
	// For details, see the design doc at
	// https://docs.google.com/document/d/1YDlGIdVTPnmUiTAavlZxBI1d9pwGQgZT7IKFKlIXohQ/edit?usp=sharing
	// The basic algorithm is to track the average size of stacks
	// and start goroutines with stack equal to that average size.
	// Starting at the average size uses at most 2x the space that an ideal algorithm would have used.
	// This is just a heuristic to avoid excessive stack growth work
	// early in a goroutine's lifetime. See issue 18138. Stacks that
	// are allocated too small can still grow, and stacks allocated
	// too large can still shrink.
	var scannedStackSize uint64
	var scannedStacks uint64
	for _, p := range allp {
		scannedStackSize += p.scannedStackSize
		scannedStacks += p.scannedStacks
		// Reset for next time
		p.scannedStackSize = 0
		p.scannedStacks = 0
	}
	if scannedStacks == 0 {
		startingStackSize = _FixedStack
		return
	}
	avg := scannedStackSize/scannedStacks + _StackGuard
	// Note: we add _StackGuard to ensure that a goroutine that
	// uses the average space will not trigger a growth.
	if avg > uint64(maxstacksize) {
		avg = uint64(maxstacksize)
	}
	if avg < _FixedStack {
		avg = _FixedStack
	}
	// Note: maxstacksize fits in 30 bits, so avg also does.
	//
	// amd64，maxstacksize的值是1000000000，向上对齐到2的幂为2^30
	startingStackSize = uint32(round2(int32(avg)))
}
