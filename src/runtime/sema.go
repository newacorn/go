// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Semaphore implementation exposed to Go.
// Intended use is provide a sleep and wakeup
// primitive that can be used in the contended case
// of other synchronization primitives.
// Thus it targets the same goal as Linux's futex,
// but it has much simpler semantics.
//
// That is, don't think of these as semaphores.
// Think of them as a way to implement sleep and wakeup
// such that every sleep is paired with a single wakeup,
// even if, due to races, the wakeup happens before the sleep.
//
// See Mullender and Cox, ``Semaphores in Plan 9,''
// https://swtch.com/semaphore.pdf

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"unsafe"
)

// Asynchronous semaphore for sync.Mutex.

// A semaRoot holds a balanced tree of sudog with distinct addresses (s.elem).
// Each of those sudog may in turn point (through s.waitlink) to a list
// of other sudogs waiting on the same address.
// The operations on the inner lists of sudogs with the same address
// are all O(1). The scanning of the top-level semaRoot list is O(log n),
// where n is the number of distinct addresses with goroutines blocked
// on them that hash to the given semaRoot.
// See golang.org/issue/17953 for a program that worked badly
// before we introduced the second level of list, and
// BenchmarkSemTable/OneAddrCollision/* for a benchmark that exercises this.
type semaRoot struct {
	// lock 用来保护这棵平衡树
	lock  mutex
	// 是正在平衡树数据结构的根，实际上平衡树的每个节点都是个sudog类型
	// 的对象。
	treap *sudog        // root of balanced tree of unique waiters.
	// 表明了树中结点的数量
	nwait atomic.Uint32 // Number of waiters. Read w/o the lock.
}
// runtime内存会通过一个大小为251的 semtable 来管理所有的
// semaphore。
var semtable semTable

// Prime to not correlate with any user patterns.
// runtime 会把semaphore放到平衡树中，而sematable存储的是251棵平衡
// 树的根，对应的数据结构为 semaRoot。
const semTabSize = 251

type semTable [semTabSize]struct {
	root semaRoot
	pad  [cpu.CacheLinePadSize - unsafe.Sizeof(semaRoot{})]byte
}
// 根据addr进行计算并映射到sematale中的一棵平衡树上，rootFor
// 将addr映射到平衡树的根。
func (t *semTable) rootFor(addr *uint32) *semaRoot {
	// 先把 addr转换成uintptr，然后对其到8字节，在对表的大小取模，
	// 结果用作数组下标。定位到某刻平衡树后，再根据sudog.elem存储的地址
	// 与信号量的地址是否相等，进一步定位到某个节点，这样就找到该信号量对应
	// 的等待队列了。
	return &t[(uintptr(unsafe.Pointer(addr))>>3)%semTabSize].root
}

//go:linkname sync_runtime_Semacquire sync.runtime_Semacquire
func sync_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile, 0, waitReasonSemacquire)
}

//go:linkname poll_runtime_Semacquire internal/poll.runtime_Semacquire
func poll_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile, 0, waitReasonSemacquire)
}

//go:linkname sync_runtime_Semrelease sync.runtime_Semrelease
func sync_runtime_Semrelease(addr *uint32, handoff bool, skipframes int) {
	semrelease1(addr, handoff, skipframes)
}

//go:linkname sync_runtime_SemacquireMutex sync.runtime_SemacquireMutex
func sync_runtime_SemacquireMutex(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncMutexLock)
}

//go:linkname sync_runtime_SemacquireRWMutexR sync.runtime_SemacquireRWMutexR
func sync_runtime_SemacquireRWMutexR(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncRWMutexRLock)
}

//go:linkname sync_runtime_SemacquireRWMutex sync.runtime_SemacquireRWMutex
func sync_runtime_SemacquireRWMutex(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncRWMutexLock)
}

//go:linkname poll_runtime_Semrelease internal/poll.runtime_Semrelease
func poll_runtime_Semrelease(addr *uint32) {
	semrelease(addr)
}

func readyWithTime(s *sudog, traceskip int) {
	if s.releasetime != 0 {
		s.releasetime = cputicks()
	}
	goready(s.g, traceskip)
}

type semaProfileFlags int

const (
	semaBlockProfile semaProfileFlags = 1 << iota
	semaMutexProfile
)

// Called from runtime.
func semacquire(addr *uint32) {
	semacquire1(addr, false, 0, 0, waitReasonSemacquire)
}
// semacquire1()
// runtime中的semaphore是可用协程使用的信号量实现，预期用它来提供一组sleep和wakeup原语
// ,目标与Linux的futex相同。也就是说，不要把它视为信号量，而是把应把它当成实现休眠和唤醒
// 的一种方式。每个sleep都与一次wakeup对应，即使因为竞争的关系，wakeup发生在sleep之前。
// 如果wakeup先调用，后调用的sleep会立即返回。
// 参数：
// addr 用作信号量的uint32型变量的地址
// lifo 表示是否采用LIFO的排队策略
// profile 与性能分析相关，表示要进行哪些种类的采样，目前有 semaBlockProfile
// 和 semaMutexProfile。
// skipframes 用来指示栽回溯跳过runtime自身的栈帧。
//
// 当要使用一个信号量时，需要提供一个记录信号量数值的变量，根据它的地址
// addr进行即使并映射到 semtable 中的一颗平衡树上， semTable.rootFor 函数专门
// 用来把 addr 映射到对应的平衡数的根。
//
// 根据addr进行计算并映射到sematale中的一棵平衡树上，rootFor
// 将addr映射到平衡树的根。
//func (t *semTable) rootFor(addr *uint32) *semaRoot {
// 先把 addr转换成uintptr，然后对其到8字节，在对表的大小取模，
// 结果用作数组下标。
//
// 定位到某刻平衡树后，再根据sudog.elem存储的地址
// 与信号量的地址是否相等，进一步定位到某个节点，这样就找到该信号量对应
// 的等待队列了。
func semacquire1(addr *uint32, lifo bool, profile semaProfileFlags, skipframes int, reason waitReason) {
	gp := getg()
	if gp != gp.m.curg {
		throw("semacquire not on the G stack")
	}

	// Easy case.
	if cansemacquire(addr) {
		return
	}

	// Harder case:
	//	increment waiter count
	//	try cansemacquire one more time, return if succeeded
	//	enqueue itself as a waiter
	//	sleep
	//	(waiter descriptor is dequeued by signaler)
	s := acquireSudog()
	root := semtable.rootFor(addr)
	t0 := int64(0)
	s.releasetime = 0
	s.acquiretime = 0
	s.ticket = 0
	// blockProfilerate  控制内存MemProfile的频率为1表示profile everything
	// profile 信号量阻塞分析
	// semaBlockProfile 常量值为1
	if profile&semaBlockProfile != 0 && blockprofilerate > 0 {
		t0 = cputicks()
		s.releasetime = -1
	}
	if profile&semaMutexProfile != 0 && mutexprofilerate > 0 {
		if t0 == 0 {
			t0 = cputicks()
		}
		s.acquiretime = t0
	}
	for {
		lockWithRank(&root.lock, lockRankRoot)
		// Add ourselves to nwait to disable "easy case" in semrelease.
		root.nwait.Add(1)
		// Check cansemacquire to avoid missed wakeup.
		if cansemacquire(addr) {
			root.nwait.Add(-1)
			unlock(&root.lock)
			break
		}
		// Any semrelease after the cansemacquire knows we're waiting
		// (we set nwait above), so go to sleep.
		// 获取失败，将关联的sudog入队列
		root.queue(addr, s, lifo)
		// 停靠
		goparkunlock(&root.lock, reason, traceEvGoBlockSync, 4+skipframes)
		// 被信号量的持有者唤醒
		// s.ticket 的值不等于0，表示释放者已经为其获得了信号量不用再尝试了
		if s.ticket != 0 || cansemacquire(addr) {
			break
		}
	}
	// 最后一次被唤醒且获得锁的时间
	if s.releasetime > 0 {
		// 从休眠到被唤醒的时间差值
		blockevent(s.releasetime-t0, 3+skipframes)
	}
	// 释放sudog结构回缓存池
	releaseSudog(s)
}

func semrelease(addr *uint32) {
	semrelease1(addr, false, 0)
}
// 参数：
// 被唤醒的协程会设置到当前P的runnext，如果 handoff 为true
// ，则当前协程会通过 goyield()让出CPU，被唤醒的协程会立即
// 得到调度。
func semrelease1(addr *uint32, handoff bool, skipframes int) {
	root := semtable.rootFor(addr)
	atomic.Xadd(addr, 1)

	// Easy case: no waiters?
	// This check must happen after the xadd, to avoid a missed wakeup
	// (see loop in semacquire).
	if root.nwait.Load() == 0 {
		// 如果观察到 root.nwait=0，那么将nwait=1的等候者
		// 绝对可以观察到addr=1，所以不用唤醒它。
		return
	}
	// 如果观察到 root.nwait!=0
	// 不能说明等待者观察到了addr=1

	// Harder case: search for a waiter and wake it.
	lockWithRank(&root.lock, lockRankRoot)
	if root.nwait.Load() == 0 {
		// 能观察到 root.nwait=0
		// 说明等待队列中没有成员

		// The count is already consumed by another goroutine,
		// so no need to wake up another goroutine.
		unlock(&root.lock)
		return
	}
	// 至此等待队列中肯定有成员
	// 因为对 nwait 的变更必须持有锁，而目前正在此锁中。
	s, t0 := root.dequeue(addr)
	if s != nil {
		root.nwait.Add(-1)
	}
	unlock(&root.lock)
	if s != nil { // May be slow or even yield, so unlock first
		// s关联的g肯定会恢复运行
		acquiretime := s.acquiretime
		if acquiretime != 0 {
			mutexevent(t0-acquiretime, 3+skipframes)
		}
		if s.ticket != 0 {
			throw("corrupted semaphore ticket")
		}
		// handoff 决定是否让出当前时间片
		// addr不一定还是1，因为其它等待者可以在没有进入锁的情况下执行
		// cansemacquire(addr)
		if handoff && cansemacquire(addr) {
			// 使用当前时间片同时获得了信号量
			s.ticket = 1
		}
		// 将s关联的g设置到当前P的nextg字段。
		readyWithTime(s, 5+skipframes)
		if s.ticket == 1 && getg().m.locks == 0 {
			// 挂起当前g，让出时间片。让上面的s.g恢复运行。
			// Direct G handoff
			// readyWithTime has added the waiter G as runnext in the
			// current P; we now call the scheduler so that we start running
			// the waiter G immediately.
			// Note that waiter inherits our time slice: this is desirable
			// to avoid having a highly contended semaphore hog the P
			// indefinitely. goyield is like Gosched, but it emits a
			// "preempted" trace event instead and, more importantly, puts
			// the current G on the local runq instead of the global one.
			// We only do this in the starving regime (handoff=true), as in
			// the non-starving case it is possible for a different waiter
			// to acquire the semaphore while we are yielding/scheduling,
			// and this would be wasteful. We wait instead to enter starving
			// regime, and then we start to do direct handoffs of ticket and
			// P.
			// See issue 33747 for discussion.
			goyield()
		}
	}
}
// *addr 等于0返回false
// 原子递减1且保证之后的结果大于等于0，则返回true。
func cansemacquire(addr *uint32) bool {
	for {
		v := atomic.Load(addr)
		if v == 0 {
			return false
		}
		// 下面的保证成功后*addr的值大于等于0。
		if atomic.Cas(addr, v, v-1) {
			return true
		}
	}
}

// queue adds s to the blocked goroutines in semaRoot.
func (root *semaRoot) queue(addr *uint32, s *sudog, lifo bool) {
	s.g = getg()
	s.elem = unsafe.Pointer(addr)
	s.next = nil
	s.prev = nil

	var last *sudog
	pt := &root.treap
	for t := *pt; t != nil; t = *pt {
		if t.elem == unsafe.Pointer(addr) {
			// Already have addr in list.
			if lifo {
				// Substitute s in t's place in treap.
				*pt = s
				s.ticket = t.ticket
				s.acquiretime = t.acquiretime
				s.parent = t.parent
				s.prev = t.prev
				s.next = t.next
				if s.prev != nil {
					s.prev.parent = s
				}
				if s.next != nil {
					s.next.parent = s
				}
				// Add t first in s's wait list.
				s.waitlink = t
				s.waittail = t.waittail
				if s.waittail == nil {
					s.waittail = t
				}
				t.parent = nil
				t.prev = nil
				t.next = nil
				t.waittail = nil
			} else {
				// Add s to end of t's wait list.
				if t.waittail == nil {
					t.waitlink = s
				} else {
					t.waittail.waitlink = s
				}
				t.waittail = s
				s.waitlink = nil
			}
			return
		}
		last = t
		if uintptr(unsafe.Pointer(addr)) < uintptr(t.elem) {
			pt = &t.prev
		} else {
			pt = &t.next
		}
	}

	// Add s as new leaf in tree of unique addrs.
	// The balanced tree is a treap using ticket as the random heap priority.
	// That is, it is a binary tree ordered according to the elem addresses,
	// but then among the space of possible binary trees respecting those
	// addresses, it is kept balanced on average by maintaining a heap ordering
	// on the ticket: s.ticket <= both s.prev.ticket and s.next.ticket.
	// https://en.wikipedia.org/wiki/Treap
	// https://faculty.washington.edu/aragon/pubs/rst89.pdf
	//
	// s.ticket compared with zero in couple of places, therefore set lowest bit.
	// It will not affect treap's quality noticeably.
	s.ticket = fastrand() | 1
	s.parent = last
	*pt = s

	// Rotate up into tree according to ticket (priority).
	for s.parent != nil && s.parent.ticket > s.ticket {
		if s.parent.prev == s {
			root.rotateRight(s.parent)
		} else {
			if s.parent.next != s {
				panic("semaRoot queue")
			}
			root.rotateLeft(s.parent)
		}
	}
}

// dequeue searches for and finds the first goroutine
// in semaRoot blocked on addr.
// If the sudog was being profiled, dequeue returns the time
// at which it was woken up as now. Otherwise now is 0.
func (root *semaRoot) dequeue(addr *uint32) (found *sudog, now int64) {
	ps := &root.treap
	s := *ps
	for ; s != nil; s = *ps {
		if s.elem == unsafe.Pointer(addr) {
			goto Found
		}
		if uintptr(unsafe.Pointer(addr)) < uintptr(s.elem) {
			ps = &s.prev
		} else {
			ps = &s.next
		}
	}
	return nil, 0

Found:
	now = int64(0)
	if s.acquiretime != 0 {
		now = cputicks()
	}
	if t := s.waitlink; t != nil {
		// Substitute t, also waiting on addr, for s in root tree of unique addrs.
		*ps = t
		t.ticket = s.ticket
		t.parent = s.parent
		t.prev = s.prev
		if t.prev != nil {
			t.prev.parent = t
		}
		t.next = s.next
		if t.next != nil {
			t.next.parent = t
		}
		if t.waitlink != nil {
			t.waittail = s.waittail
		} else {
			t.waittail = nil
		}
		t.acquiretime = now
		s.waitlink = nil
		s.waittail = nil
	} else {
		// Rotate s down to be leaf of tree for removal, respecting priorities.
		for s.next != nil || s.prev != nil {
			if s.next == nil || s.prev != nil && s.prev.ticket < s.next.ticket {
				root.rotateRight(s)
			} else {
				root.rotateLeft(s)
			}
		}
		// Remove s, now a leaf.
		if s.parent != nil {
			if s.parent.prev == s {
				s.parent.prev = nil
			} else {
				s.parent.next = nil
			}
		} else {
			root.treap = nil
		}
	}
	s.parent = nil
	s.elem = nil
	s.next = nil
	s.prev = nil
	s.ticket = 0
	return s, now
}

// rotateLeft rotates the tree rooted at node x.
// turning (x a (y b c)) into (y (x a b) c).
func (root *semaRoot) rotateLeft(x *sudog) {
	// p -> (x a (y b c))
	p := x.parent
	y := x.next
	b := y.prev

	y.prev = x
	x.parent = y
	x.next = b
	if b != nil {
		b.parent = x
	}

	y.parent = p
	if p == nil {
		root.treap = y
	} else if p.prev == x {
		p.prev = y
	} else {
		if p.next != x {
			throw("semaRoot rotateLeft")
		}
		p.next = y
	}
}

// rotateRight rotates the tree rooted at node y.
// turning (y (x a b) c) into (x a (y b c)).
func (root *semaRoot) rotateRight(y *sudog) {
	// p -> (y (x a b) c)
	p := y.parent
	x := y.prev
	b := x.next

	x.next = y
	y.parent = x
	y.prev = b
	if b != nil {
		b.parent = y
	}

	x.parent = p
	if p == nil {
		root.treap = x
	} else if p.prev == y {
		p.prev = x
	} else {
		if p.next != y {
			throw("semaRoot rotateRight")
		}
		p.next = x
	}
}

// notifyList is a ticket-based notification list used to implement sync.Cond.
//
// It must be kept in sync with the sync package.
type notifyList struct {
	// wait is the ticket number of the next waiter. It is atomically
	// incremented outside the lock.
	wait atomic.Uint32

	// notify is the ticket number of the next waiter to be notified. It can
	// be read outside the lock, but is only written to with lock held.
	//
	// Both wait & notify can wrap around, and such cases will be correctly
	// handled as long as their "unwrapped" difference is bounded by 2^31.
	// For this not to be the case, we'd need to have 2^31+ goroutines
	// blocked on the same condvar, which is currently not possible.
	notify uint32

	// List of parked waiters.
	lock mutex
	head *sudog
	tail *sudog
}

// less checks if a < b, considering a & b running counts that may overflow the
// 32-bit range, and that their "unwrapped" difference is always less than 2^31.
func less(a, b uint32) bool {
	return int32(a-b) < 0
}

// notifyListAdd adds the caller to a notify list such that it can receive
// notifications. The caller must eventually call notifyListWait to wait for
// such a notification, passing the returned ticket number.
//
//go:linkname notifyListAdd sync.runtime_notifyListAdd
func notifyListAdd(l *notifyList) uint32 {
	// This may be called concurrently, for example, when called from
	// sync.Cond.Wait while holding a RWMutex in read mode.
	return l.wait.Add(1) - 1
}

// notifyListWait waits for a notification. If one has been sent since
// notifyListAdd was called, it returns immediately. Otherwise, it blocks.
//
//go:linkname notifyListWait sync.runtime_notifyListWait
func notifyListWait(l *notifyList, t uint32) {
	lockWithRank(&l.lock, lockRankNotifyList)

	// Return right away if this ticket has already been notified.
	if less(t, l.notify) {
		unlock(&l.lock)
		return
	}

	// Enqueue itself.
	s := acquireSudog()
	s.g = getg()
	s.ticket = t
	s.releasetime = 0
	t0 := int64(0)
	if blockprofilerate > 0 {
		t0 = cputicks()
		s.releasetime = -1
	}
	if l.tail == nil {
		l.head = s
	} else {
		l.tail.next = s
	}
	l.tail = s
	goparkunlock(&l.lock, waitReasonSyncCondWait, traceEvGoBlockCond, 3)
	if t0 != 0 {
		blockevent(s.releasetime-t0, 2)
	}
	releaseSudog(s)
}

// notifyListNotifyAll notifies all entries in the list.
//
//go:linkname notifyListNotifyAll sync.runtime_notifyListNotifyAll
func notifyListNotifyAll(l *notifyList) {
	// Fast-path: if there are no new waiters since the last notification
	// we don't need to acquire the lock.
	if l.wait.Load() == atomic.Load(&l.notify) {
		return
	}

	// Pull the list out into a local variable, waiters will be readied
	// outside the lock.
	lockWithRank(&l.lock, lockRankNotifyList)
	s := l.head
	l.head = nil
	l.tail = nil

	// Update the next ticket to be notified. We can set it to the current
	// value of wait because any previous waiters are already in the list
	// or will notice that they have already been notified when trying to
	// add themselves to the list.
	atomic.Store(&l.notify, l.wait.Load())
	unlock(&l.lock)

	// Go through the local list and ready all waiters.
	for s != nil {
		next := s.next
		s.next = nil
		readyWithTime(s, 4)
		s = next
	}
}

// notifyListNotifyOne notifies one entry in the list.
//
//go:linkname notifyListNotifyOne sync.runtime_notifyListNotifyOne
func notifyListNotifyOne(l *notifyList) {
	// Fast-path: if there are no new waiters since the last notification
	// we don't need to acquire the lock at all.
	if l.wait.Load() == atomic.Load(&l.notify) {
		return
	}

	lockWithRank(&l.lock, lockRankNotifyList)

	// Re-check under the lock if we need to do anything.
	t := l.notify
	if t == l.wait.Load() {
		unlock(&l.lock)
		return
	}

	// Update the next notify ticket number.
	atomic.Store(&l.notify, t+1)

	// Try to find the g that needs to be notified.
	// If it hasn't made it to the list yet we won't find it,
	// but it won't park itself once it sees the new notify number.
	//
	// This scan looks linear but essentially always stops quickly.
	// Because g's queue separately from taking numbers,
	// there may be minor reorderings in the list, but we
	// expect the g we're looking for to be near the front.
	// The g has others in front of it on the list only to the
	// extent that it lost the race, so the iteration will not
	// be too long. This applies even when the g is missing:
	// it hasn't yet gotten to sleep and has lost the race to
	// the (few) other g's that we find on the list.
	for p, s := (*sudog)(nil), l.head; s != nil; p, s = s, s.next {
		if s.ticket == t {
			n := s.next
			if p != nil {
				p.next = n
			} else {
				l.head = n
			}
			if n == nil {
				l.tail = p
			}
			unlock(&l.lock)
			s.next = nil
			readyWithTime(s, 4)
			return
		}
	}
	unlock(&l.lock)
}

//go:linkname notifyListCheck sync.runtime_notifyListCheck
func notifyListCheck(sz uintptr) {
	if sz != unsafe.Sizeof(notifyList{}) {
		print("runtime: bad notifyList size - sync=", sz, " runtime=", unsafe.Sizeof(notifyList{}), "\n")
		throw("bad notifyList size")
	}
}

//go:linkname sync_nanotime sync.runtime_nanotime
func sync_nanotime() int64 {
	return nanotime()
}
