// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// There is a modified copy of this file in runtime/rwmutex.go.
// If you make any changes here, see if you should make them there.

// A RWMutex is a reader/writer mutual exclusion lock.
// The lock can be held by an arbitrary number of readers or a single writer.
// The zero value for a RWMutex is an unlocked mutex.
//
// A RWMutex must not be copied after first use.
//
// If a goroutine holds a RWMutex for reading and another goroutine might
// call Lock, no goroutine should expect to be able to acquire a read lock
// until the initial read lock is released. In particular, this prohibits
// recursive read locking. This is to ensure that the lock eventually becomes
// available; a blocked Lock call excludes new readers from acquiring the
// lock.
//
// In the terminology of the Go memory model,
// the n'th call to Unlock “synchronizes before” the m'th call to Lock
// for any n < m, just as for Mutex.
// For any call to RLock, there exists an n such that
// the n'th call to Unlock “synchronizes before” that call to RLock,
// and the corresponding call to RUnlock “synchronizes before”
// the n+1'th call to Lock.
// 谁能够阻塞谁？
// 1. 写阻塞读：
// 当写锁获取者阻塞在 rw.writerSem 上时或者写锁已经获得都会面调用的 RLock 调用。
// 2. 读阻塞写：
// 读锁持有者会阻塞之后的写锁获取者。其实进入阻塞的读锁也有会延迟阻塞在 rw.m 上的写错等待者。
// 因为写锁在解锁 rw.m.Unlock 之前会先将当下的所有读锁唤醒。当下的读锁唤醒完后，rw.m上的
// 写锁大概率又会阻塞在 rw.writerSem 上。因为读锁被唤醒后一般不会立即释放锁，且读锁一般很多。
//
// 谁解锁谁？
// RLock 获得的是读锁，如果成功返回后，在不需要读锁的情况下只能执行 RUnlock。
// Lock 获得的是写锁，如果成功返回之后，在不需要锁的情况下只能执行 Unlock。
//
// 如果使用 RLock 获得了读锁，确使用 Unlock 来尝试解锁，会导致逻辑混乱甚至导致panic(如果当期没有阻塞的
// 写锁的话)。
// 同样使用 Lock 获得了写锁，接着调用 RUnlock 来尝试解锁，会导致逻辑混乱。
//
// 如果目前存在持有读锁者，此时调用了 Lock，这 Lock 调用会进入等待队列，同时这个 Lock 也会阻塞之后的 RLock
// 调用（即使这个 Lock 调用因为之前的读锁而阻塞在 rw.writerSem 上)。其调用之前的读锁持有者
// 中最后一个释放读锁的读锁持有者负责唤醒它。在 Lock 调用被唤醒返回之后，当它调用 Unlock 时，会将
// 当前因为 Lock 调用之后而阻塞的所有读锁等候者唤醒，然后才会调用 rw.w.Unlock，之后阻塞在 rw.m 上的写锁才会
// 有一个进入到 rw.writerSem 上排队或者成功从 Lock 返回。
//
// Unlock 解锁后 RLock 会获得锁，而不是 Lock 获得锁
// RUnlock 解锁后 Loc才能获得锁。
type RWMutex struct {
	// 维护写锁等待者排队，保证一次只有一个写锁持有者(w上的写锁持有者)与读锁获取者和读锁持有者进行交互。
	// 这也造成了写锁存在两种阻塞，一种阻塞在 w 上，一种阻塞在 writerSem 上。
	w           Mutex        // held if there are pending writers
	// 已经获得 w 上的写锁但在它前面还有读锁持有者，写锁持有者会排队在此信号量上。
	// 只会有一个写锁排队在此信号量上，因为只有过了 w 这一关才有可能排队在此信号量上，
	// 又因为 w 为互斥锁，上锁和释放锁需成对出现，此信号量上的等待者需被唤醒然后调用 Unlock
	// 才会让后来的写锁从 w 阻塞上唤醒。
	//
	// Unlock , 其他写锁等待者是过不了 w 这一关的。
	writerSem   uint32       // semaphore for writers to wait for completing readers
	//
	// RLock 的调用者，可能会排队在此信号量上，如果它之前有写锁持有者或者写锁等待者(在 writerSem 上排队)。
	// Unlock 调用中，可能会唤醒这些阻塞在此信号量上的读锁等待者(如果存在的话)。
	readerSem   uint32       // semaphore for readers to wait for completing writers
	//
	// 读锁持有者和排在 readerSem 信号量上的读锁等待者的总和。(不考虑写锁通过此计数来通知之后的读锁获取者
	// 有一个写锁在它们前面的这种情况)。
	// 在 RLock 函数调用中递增，在 RUnlock 函数调用中递减。
	// 在 Lock 函数调用中减去 rwmutexMaxReaders, 在 Unlock 函数调用中加上 rwmutexMaxReaders。
	//
	// Unlock 在调用时会根据此计数唤醒在 raderSem 上的相应数量的读锁等待者。
	readerCount atomic.Int32 // number of pending readers
	// 读锁持有者数量，
	// 由 Lock 函数调用时进行计数增加。由 RUnlock 函数调用时进行计数递减。
	//
	// RUnlock 在调用时通过 readerCount 是否为负值来判断是否存在在 writerSem 上的写锁等待者，如果
	// 存在，当 readerWait 计数减少到0时会唤醒 writerSem 上的一个写锁等待者。
	readerWait  atomic.Int32 // number of departing readers
}

const rwmutexMaxReaders = 1 << 30

// Happens-before relationships are indicated to the race detector via:
// - Unlock  -> Lock:  readerSem
// - Unlock  -> RLock: readerSem
// - RUnlock -> Lock:  writerSem
//
// The methods below temporarily disable handling of race synchronization
// events in order to provide the more precise model above to the race
// detector.
//
// For example, atomic.AddInt32 in RLock should not appear to provide
// acquire-release semantics, which would incorrectly synchronize racing
// readers, thus potentially missing races.

// RLock locks rw for reading.
//
// It should not be used for recursive read locking; a blocked Lock
// call excludes new readers from acquiring the lock. See the
// documentation on the RWMutex type.
func (rw *RWMutex) RLock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	if rw.readerCount.Add(1) < 0 {
		// 至此：
		// 说明在调用 Rlock时，在它之前肯定有一个写锁持有者。即rw.readerCount.Add(1) happen before rw.Ulock()
		// 所以需要在 rw.readerSem 信号量上排队。
		//
		// 出现小于0的情况是因为：
		// 1. rw.readerCount的值在Lock调用时会被设置为:rw.readerCount- rwmutexMaxReaders
		// 2. 前提：当rwmutexMaxReaders - n-1 > 0，n为目前读锁持有者或尝试持有者(准备进入等待队列或已经进入等待队列)的和。
		//    这样上面if语句的正确性就可以得到满足。一般不会有那么多读锁所以肯定可以满足。
		//
		// A writer is pending, wait for it.
		//
		// 唤醒条件：
		// 之前存在一个阻塞等待中的写锁(已经获得了写锁,但在等待其前面的读锁全部释放)。
		// 这个读锁等候者要等待那个写锁持有者释放写锁之后才能唤醒并获得锁。
		// 因为那个写锁持有者在释放写锁时会唤醒它之后的读锁等待者。
		//
		runtime_SemacquireRWMutexR(&rw.readerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
}

// TryRLock tries to lock rw for reading and reports whether it succeeded.
//
// Note that while correct uses of TryRLock do exist, they are rare,
// and use of TryRLock is often a sign of a deeper problem
// in a particular use of mutexes.
//
// 获取失败不会改变 rw 的状态。
func (rw *RWMutex) TryRLock() bool {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	for {
		c := rw.readerCount.Load()
		if c < 0 {
			// c<0 说明 1. rw.Lock happen before rw.TryRLock
			//         2. rw.Unlock happen after rw.TryRlock
			// 所以直接返回false。
			if race.Enabled {
				race.Enable()
			}
			return false
		}

		// 其实这里只需保证 readerCount还是非负值即可，然后递增1。
		// 但这是两个步骤无法做这样的原子保证。
		// 所以只能采用下面的 cas 方式。
		// 如果变更失败，说明readerCount在上面的读取到下面的替换
		// 之间发生了变更，需重新获取。
		if rw.readerCount.CompareAndSwap(c, c+1) {
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(&rw.readerSem))
			}
			return true
		}
	}
}

// RUnlock undoes a single RLock call;
// it does not affect other simultaneous readers.
// It is a run-time error if rw is not locked for reading
// on entry to RUnlock.
func (rw *RWMutex) RUnlock() {
	if race.Enabled {
		_ = rw.w.state
		race.ReleaseMerge(unsafe.Pointer(&rw.writerSem))
		race.Disable()
	}
	if r := rw.readerCount.Add(-1); r < 0 {
		// 说明此读锁持有者之后有一个休眠中的写锁。
		//
		// Outlined slow-path to allow the fast-path to be inlined
		// 需要执行下面的函数去尝试唤醒这个阻塞的写锁。其实只有写锁前面的最后一个读锁可以唤醒它。
		rw.rUnlockSlow(r)
	}
	if race.Enabled {
		race.Enable()
	}
}

func (rw *RWMutex) rUnlockSlow(r int32) {
	if r+1 == 0 || r+1 == -rwmutexMaxReaders {
		// r+1 == 0，没有调用 RLock 的情况下调用了 RUnlock
		// r+1 == -rwmutexMaxReaders 调用了Lock后，直接调用了 RUnlock
		race.Enable()
		fatal("sync: RUnlock of unlocked RWMutex")
	}
	// A writer is pending.
	if rw.readerWait.Add(-1) == 0 {
		// 此读锁持有者是最后一个，则唤醒休眠中的写锁。
		// The last reader unblocks the writer.
		//
		// 将写锁关联的写成设置到当前P的nextg字段并返回。
		runtime_Semrelease(&rw.writerSem, false, 1)
	}
}

// Lock locks rw for writing.
// If the lock is already locked for reading or writing,
// Lock blocks until the lock is available.
func (rw *RWMutex) Lock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// First, resolve competition with other writers.
	// 只阻碍写锁，写锁和写锁通过 rw.w 来进行互斥操作。
	rw.w.Lock()
	// Announce to readers there is a pending writer.
	//
	// 至此: 虽然获得了写锁，但是如果在此之前存在读锁持有者，需休眠
	// 等待读锁之前最后一个读锁持有者唤醒。
	//
	// 告知未来的读锁它们之前还有个阻塞的写锁。
	r := rw.readerCount.Add(-rwmutexMaxReaders) + rwmutexMaxReaders
	// 此条指令执行之后，再执行 RLock 的读锁尝试获取者都会在 rw.readerSem 信号量上排队。
	//
	// Wait for active readers.
	//
	// r表示此写锁之前还有r个读锁持有者。
	// 不过这只是个短暂的状态，因为此时可能存在读锁执行了 RUnlock 并从中返回，此时读锁此后者就不是
	// r了，不过读锁解锁时会递减 rw.readerWait 且上面的指令执行之后不会有新的读锁持有者了 ，这样
	// 下面通执行 rw.readerWait.Add(r) 又得到了正确的读锁持有者计数即 rw.readerWait。
	//
	// Wait for active readers.
	// rw.readerWait表示,有这么多个读锁获持有者，当这些读锁都释放了，此写锁才会被被唤醒。
	// r不等于0执行，rw.readerWait.Addr(r)，才有意义。因为没有读锁持有者可以直接返回了。
	// 因为在获取r到下面rw.readerWait.Add(r) != 0 执行时，可能读锁持有者都释放完锁了，
	// 所以有可能 rw.readerWait.Add(r) = 0，这样此写锁获取者就不必进入到信号量上排队了。
	if r != 0 && rw.readerWait.Add(r) != 0 {
		// 此写锁等候者之前还存在至少一个读锁持有者。
		// 休眠，等待之前的最后一个读锁持有者释放时唤醒。
		runtime_SemacquireRWMutex(&rw.writerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
}

// TryLock tries to lock rw for writing and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
//
// 获取失败不会改变 rw 的状态。
func (rw *RWMutex) TryLock() bool {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// 如果已经有写锁持有者了，所以可以直接返回。
	if !rw.w.TryLock() {
		if race.Enabled {
			race.Enable()
		}
		return false
	}
	// 如果下面替换成功说明前没有读锁持有者，可顺利获取锁。
	if !rw.readerCount.CompareAndSwap(0, -rwmutexMaxReaders) {
		// 存在读锁持有者，释放rw.m 上的写锁。
		rw.w.Unlock()
		if race.Enabled {
			race.Enable()
		}
		return false
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
	return true
}

// Unlock unlocks rw for writing. It is a run-time error if rw is
// not locked for writing on entry to Unlock.
//
// As with Mutexes, a locked RWMutex is not associated with a particular
// goroutine. One goroutine may RLock (Lock) a RWMutex and then
// arrange for another goroutine to RUnlock (Unlock) it.
func (rw *RWMutex) Unlock() {
	if race.Enabled {
		_ = rw.w.state
		race.Release(unsafe.Pointer(&rw.readerSem))
		race.Disable()
	}

	// Announce to readers there is no active writer.
	//
	// 告诉之后新到来的读锁等候者已经不存在写锁等候者了。
	// 因为就有原子性，在此指令之前的执行 rw.readerCount.Add(1)肯定会小于0
	// 会一个不漏的计数到r中。
	// 需要被唤醒。在此指令之后执行的 rw.readerCount.Add(1)肯定会大于0，不
	// 需要唤醒，也不会计数到r中。
	r := rw.readerCount.Add(rwmutexMaxReaders)
	// 其实这样会存在一种情况：就是它之前的读锁等候者还没唤醒，但此刻新来的读锁获取者
	// 又能立即获得到读锁。不过这个时间段短因为接下来就会唤醒在 rw.readerSema 信号
	// 量上的所有等待者了。
	if r >= rwmutexMaxReaders {
		// 没有调用 Lock，就调用了 Unlock
		race.Enable()
		fatal("sync: Unlock of unlocked RWMutex")
	}
	// Unblock blocked readers, if any.
	for i := 0; i < int(r); i++ {
		//
		// 这个 r个读锁等待者肯定可以观察到 atomic.AddInt32(&rw.readerCount, 1) < 0
		//
		// 并不一定存在 r个休眠中的读锁等待者，可能有些是刚进来的读锁，它们在执行 rumtime_Semacquire()
		// 前，已经递增了对应的信号量，这样它们会立即从 rumtime_Semacquire()中返回。
		//
		// 因为所有的这种信号量的 wakeup 和 sleep 是一对一的，无论先后，所以即使下面的部分wakeup在上面的
		// 部分sleep之前也没关系。这样的sleep到时会直接返回。
		runtime_Semrelease(&rw.readerSem, false, 0)
	}
	// Allow other writers to proceed.
	// 如果存在一个写锁等待者，它在唤醒后通过变更 rw.readerCount 来阻塞之后的读锁获取者继续获取读锁。
	rw.w.Unlock()
	if race.Enabled {
		race.Enable()
	}
}

// RLocker returns a Locker interface that implements
// the Lock and Unlock methods by calling rw.RLock and rw.RUnlock.
func (rw *RWMutex) RLocker() Locker {
	return (*rlocker)(rw)
}

type rlocker RWMutex
// 只暴露读锁相关的加锁和解锁接口
func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }
