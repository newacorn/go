// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go select statements.

import (
	"internal/abi"
	"unsafe"
)

const debugSelect = false

// Select case descriptor.
// Known to compiler.
// Changes here must also be made in src/cmd/compile/internal/walk/select.go's scasetype.
type scase struct {
	c    *hchan         // chan
	elem unsafe.Pointer // data element
}

var (
	chansendpc = abi.FuncPCABIInternal(chansend)
	chanrecvpc = abi.FuncPCABIInternal(chanrecv)
)

func selectsetpc(pc *uintptr) {
	*pc = getcallerpc()
}


// 按 lockorder 中的顺序对通道进行上锁，相同的通道只上锁一次。
func sellock(scases []scase, lockorder []uint16) {
	var c *hchan
	for _, o := range lockorder {
		c0 := scases[o].c
		if c0 != c {
			c = c0
			lock(&c.lock)
		}
	}
}

// 按上锁顺序相反的顺序进行解锁，相同通道只解锁一次。
func selunlock(scases []scase, lockorder []uint16) {
	// We must be very careful here to not touch sel after we have unlocked
	// the last lock, because sel can be freed right after the last unlock.
	// Consider the following situation.
	// First M calls runtime·park() in runtime·selectgo() passing the sel.
	// Once runtime·park() has unlocked the last lock, another M makes
	// the G that calls select runnable again and schedules it for execution.
	// When the G runs on another M, it locks all the locks and frees sel.
	// Now if the first M touches sel, it will access freed memory.
	for i := len(lockorder) - 1; i >= 0; i-- {
		c := scases[lockorder[i]].c
		if i > 0 && c == scases[lockorder[i-1]].c {
			continue // will unlock it on the next iteration
		}
		unlock(&c.lock)
	}
}
// 挂起gp，并为select多路复用中关联的所有通道解锁。
// 因为第一个sudog保存在gp.waitlink上，所以传入gp即可。
func selparkcommit(gp *g, _ unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	// Set activeStackChans here instead of before we try parking
	// because we could self-deadlock in stack growth on a
	// channel lock.
	gp.activeStackChans = true
	// Mark that it's safe for stack shrinking to occur now,
	// because any thread acquiring this G's stack for shrinking
	// is guaranteed to observe activeStackChans after this store.
	gp.parkingOnChan.Store(false)
	// Make sure we unlock after setting activeStackChans and
	// unsetting parkingOnChan. The moment we unlock any of the
	// channel locks we risk gp getting readied by a channel operation
	// and so gp could continue running before everything before the
	// unlock is visible (even to gp itself).

	// This must not access gp's stack (see gopark). In
	// particular, it must not access the *hselect. That's okay,
	// because by the time this is called, gp.waiting has all
	// channels in lock order.
	var lastc *hchan
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc && lastc != nil {
			// As soon as we unlock the channel, fields in
			// any sudog with that channel may change,
			// including c and waitlink. Since multiple
			// sudogs may have the same channel, we unlock
			// only after we've passed the last instance
			// of a channel.
			unlock(&lastc.lock)
		}
		lastc = sg.c
	}
	if lastc != nil {
		unlock(&lastc.lock)
	}
	return true
}
// 对于nil的通道会当前协程会别永久挂起。
func block() {
	gopark(nil, nil, waitReasonSelectNoCases, traceEvGoStop, 1) // forever
}

// selectgo implements the select statement.
//
// cas0 points to an array of type [ncases]scase, and order0 points to
// an array of type [2*ncases]uint16 where ncases must be <= 65536.
// Both reside on the goroutine's stack (regardless of any escaping in
// selectgo).
//
// For race detector builds, pc0 points to an array of type
// [ncases]uintptr (also on the stack); for other builds, it's set to
// nil.
//
// selectgo returns the index of the chosen scase, which matches the
// ordinal position of its respective select{recv,send,default} call.
// Also, if the chosen scase was a receive operation, it reports whether
// a value was received.
// selectgo()
// 包含两个或两个以上分支的select语句会被编译成 selectgo函数。
// 编译器会将select多路复用编译成包含多个跳转指令的指令语句，根据 selectgo 返回的case索引
// 跳转到对应的case分支，执行case分支下的指令。值的赋值操作与普通通道操作一样在具体的recv和
// send时已经通过指针进行复制了。
//
// 参数：
// cas0: 指向一个数组的起始位置，数组里装的是 select 中所有的case分支，被按照 send在前recv在后
//       的顺序。元素类型为 scase。
// order0: 指向一个大小等于case分支数量两倍的 uint16 数组，实际上是作为两个大小相等的数组来
// 		   用的。前一个用来对所有case中channel的轮询操作进行乱序，后一个用来对所有case中channel的
//         加锁操作进行排序。数组元素是在 cas0 指向数组的索引值。
//         轮询需要是乱序的，避免每次select都按照case的顺序响应，对后面的case不公平，而加锁顺序则需要
//         按固定算法排序，按顺序加锁才能避免死锁。
// pc0: 和 race 检测相关
// nsends和nrecvs: 分别表在cas0 数组中执行send操作和recv操作的case分支的个数。
//                 这样根据cas0中的索引值就可以判断对应的case分支操作是写通道还是读通道了。selectgo
//                 代码中多处用到了这样的判断。i<nsends为通道写操作，否则为通道读操作。
//
// block 表示是否想要阻塞等待，对应到代码中就是，有default分支在编译时此参数值为true，否则为false。
//
// 返回值：
// int型的第一个返回值: 表示最终哪个case分支被执行了，对应cas0下标。如果因为不想阻塞而返回
// 					  则这个值是-1。
// bool 类型的第二个返回值: 在对应的case分支执行的是recv操作时，用来表示费解接收到了一个值，而不是因为
//                        通道关闭得到一个零值。只对通道接收操作有意义，非接收操作总是为false。
//
func selectgo(cas0 *scase, order0 *uint16, pc0 *uintptr, nsends, nrecvs int, block bool) (int, bool) {
	if debugSelect {
		print("select: cas0=", cas0, "\n")
	}

	// NOTE: In order to maintain a lean stack size, the number of scases
	// is capped at 65536.
	cas1 := (*[1 << 16]scase)(unsafe.Pointer(cas0))
	order1 := (*[1 << 17]uint16)(unsafe.Pointer(order0))

	ncases := nsends + nrecvs
	scases := cas1[:ncases:ncases]
	// 检索顺序
	// 用来对所有 case 中 channel 的轮询操作进行乱序。
	pollorder := order1[:ncases:ncases]
	// 上锁顺序
	// 对所有case中channel的加锁操作进行排序。轮询操作需要的是乱序，避免每次select都按照固定
	// 的顺序响应，对后面的case来讲是不公平的，而加锁顺序需要按固定算法排序，按顺序加锁才能避免
	// 死锁。
	lockorder := order1[ncases:][:ncases:ncases]
	// NOTE: pollorder/lockorder's underlying array was not zero-initialized by compiler.

	// Even when raceenabled is true, there might be select
	// statements in packages compiled without -race (e.g.,
	// ensureSigM in runtime/signal_unix.go).
	var pcs []uintptr
	if raceenabled && pc0 != nil {
		pc1 := (*[1 << 16]uintptr)(unsafe.Pointer(pc0))
		pcs = pc1[:ncases:ncases]
	}
	casePC := func(casi int) uintptr {
		if pcs == nil {
			return 0
		}
		return pcs[casi]
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	// The compiler rewrites selects that statically have
	// only 0 or 1 cases plus default into simpler constructs.
	//
	// The only way we can end up with such small sel.ncase
	// values here is for a larger select in which most channels
	// have been nilled out.
	//
	// The general code handles those
	// cases correctly, and they are rare enough not to bother optimizing (and needing to test).
	// generate permuted order
	//
	// 随机排序scases切片的索引，排除nil channel关联的
	// scase，排序结果保存在pollorder中。
	norder := 0
	for i := range scases {
		cas := &scases[i]

		// Omit cases without channels from the poll and lock orders.
		if cas.c == nil {
			cas.elem = nil // allow GC
			continue
		}

		j := fastrandn(uint32(norder + 1))
		pollorder[norder] = pollorder[j]
		pollorder[j] = uint16(i)
		norder++
	}
	// lockorder 大小与 pollorder 大小相同。
	// 排除 scase.c=nil 的scase。
	pollorder = pollorder[:norder]
	lockorder = lockorder[:norder]

	// sort the cases by Hchan address to get the locking order.
	// simple heap sort, to guarantee n log n time and constant stack footprint.
	//
	// lockorder中的case按case.c(hchan)地址排序，这样相同的hchan关联的scase的索引靠在一起。
	// 主要为了在上锁时，每个hchan只上锁一次。
	// 解锁同理。
	for i := range lockorder {
		j := i
		// Start with the pollorder to permute cases on the same channel.
		c := scases[pollorder[i]].c
		for j > 0 && scases[lockorder[(j-1)/2]].c.sortkey() < c.sortkey() {
			k := (j - 1) / 2
			lockorder[j] = lockorder[k]
			j = k
		}
		lockorder[j] = pollorder[i]
	}
	for i := len(lockorder) - 1; i >= 0; i-- {
		o := lockorder[i]
		c := scases[o].c
		lockorder[i] = lockorder[0]
		j := 0
		for {
			k := j*2 + 1
			if k >= i {
				break
			}
			if k+1 < i && scases[lockorder[k]].c.sortkey() < scases[lockorder[k+1]].c.sortkey() {
				k++
			}
			if c.sortkey() < scases[lockorder[k]].c.sortkey() {
				lockorder[j] = lockorder[k]
				j = k
				continue
			}
			break
		}
		lockorder[j] = o
	}

	if debugSelect {
		for i := 0; i+1 < len(lockorder); i++ {
			if scases[lockorder[i]].c.sortkey() > scases[lockorder[i+1]].c.sortkey() {
				print("i=", i, " x=", lockorder[i], " y=", lockorder[i+1], "\n")
				throw("select: broken sort")
			}
		}
	}

	// lock all the channels involved in the select
	// 按 lockorder 中 hchan 出现的顺序对所有chan进行上锁，已上锁过的hchan者跳过
	// 通过hchan地址来判断case是否相同。
	sellock(scases, lockorder)

	var (
		// 当前协程
		gp     *g
		// 当期协程被通道操作唤醒时保存在 gp.param 字段上的值
		sg     *sudog
		// k.c
		c      *hchan
		// 遍历时可以复用
		k      *scase
		// 将sudog从各个通道队列中移除时用到了(遍历时可以复用)。
		sglist *sudog
		// 将sudog从各个通道队列中移除时用到了(遍历时可以复用)。
		sgnext *sudog
		// 通道缓存元素的地址，这个元素是要赋值给接收变量的。
		qp     unsafe.Pointer
		// 构建 gp.waitlink 链表时用到了。
		nextp  **sudog
	)

	// pass 1 - look for something already waiting
	var casi int
	var cas *scase
	var caseSuccess bool
	var caseReleaseTime int64 = -1
	var recvOK bool
	//
	// 轮询，每个case操作，执行一个不用阻塞的case操作。
	for _, casei := range pollorder {
		casi = int(casei)
		cas = &scases[casi]
		c = cas.c

		if casi >= nsends {
			// select中关于c的操作为接收操作。
			sg = c.sendq.dequeue()
			if sg != nil {
				// c的没有缓存或者缓存已满，发送队列不为空。
				goto recv
			}
			if c.qcount > 0 {
				// c有缓存且缓存不为空。
				goto bufrecv
			}
			if c.closed != 0 {
				// c没有缓存或者缓存区为空，c已经关闭了。
				goto rclose
			}
		} else {
			// select中关于c的操作为发送操作。
			if raceenabled {
				racereadpc(c.raceaddr(), casePC(casi), chansendpc)
			}
			if c.closed != 0 {
				// 通道已经关闭。
				goto sclose
			}
			sg = c.recvq.dequeue()
			if sg != nil {
				// c没有缓存，接收队列不为空。
				goto send
			}
			if c.qcount < c.dataqsiz {
				// c有缓存，且缓存没满，接收队列肯定为空。
				goto bufsend
			}
		}
	}

	if !block {
		// 解锁
		selunlock(scases, lockorder)
		casi = -1
		goto retc
	}
	// 至此：所有的case操作都需要阻塞，且持有了所有通道的锁。

	// pass 2 - enqueue on all chans
	gp = getg()
	if gp.waiting != nil {
		// 至此：
		// 当前协程肯定应该没有在其它通道上阻塞
		throw("gp.waiting != nil")
	}
	nextp = &gp.waiting
	// 按 lockorder 中的排序，对于每个case，新建一个关联当前协程的sudog
	// 然后根据这个case是通道读还是通道写操作，将sudog放入到case关联通道的
	// recvq或sendq队列中。
	//
	// 并且在当前协程的 waiting 字段处存储了在lockorder排列第一的case关联
	// 关联的sudog，排列第一的sudog的waiting字段又会存储排列第二的sudog的
	// 地址，以此类推。这样将所有的sudog按上锁的顺序串联在了一起。
	//
	// select中的sudog与普通通道读写操作的sudog不同在于其 select 字段为true。
	//
	for _, casei := range lockorder {
		casi = int(casei)
		cas = &scases[casi]
		c = cas.c
		sg := acquireSudog()
		sg.g = gp
		sg.isSelect = true
		// No stack splits between assigning elem and enqueuing
		// sg on gp.waiting where copystack can find it.
		sg.elem = cas.elem
		sg.releasetime = 0
		if t0 != 0 {
			sg.releasetime = -1
		}
		sg.c = c
		// Construct waiting list in lock order.
		*nextp = sg
		nextp = &sg.waitlink

		if casi < nsends {
			// select中casi为写操作
			c.sendq.enqueue(sg)
		} else {
			// select中casi为读操作
			c.recvq.enqueue(sg)
		}
	}

	// wait for someone to wake us up
	gp.param = nil
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	gp.parkingOnChan.Store(true)
	gopark(selparkcommit, nil, waitReasonSelect, traceEvGoBlockSelect, 1)
	gp.activeStackChans = false

	// 唤醒后重新上锁，目的是为了将所有放入case关联通道的相应队列的sudog移除。
	// 而且某些字段的修改必须在锁的保护下，比如 gp.selectDone 。
	sellock(scases, lockorder)

	// 恢复 selectDone，在竞争唤醒当前p时被设置为1。
	gp.selectDone.Store(0)
	// 由唤醒方设置的。
	sg = (*sudog)(gp.param)
	gp.param = nil

	// pass 3 - dequeue from unsuccessful chans
	// otherwise they stack up on quiet channels
	// record the successful case, if any.
	// We singly-linked up the SudoGs in lock order.
	casi = -1
	cas = nil
	caseSuccess = false
	sglist = gp.waiting
	// Clear all elem before unlinking from gp.waiting.
	// 在将入队的sudog从相应通道的相应队列中取出之前，先清空一些
	// 字段。
	for sg1 := gp.waiting; sg1 != nil; sg1 = sg1.waitlink {
		sg1.isSelect = false
		sg1.elem = nil
		sg1.c = nil
	}
	gp.waiting = nil

	// 按上锁的顺序，将放入case相关通道的相关队列的sudog移除。
	for _, casei := range lockorder {
		k = &scases[casei]
		if sg == sglist {
			// 唤醒此协程的通道操作在唤醒当前协程时已经将通道相关队列的
			// sudog弹出赋值给当前协程的param字段。
			//
			// sg has already been dequeued by the G that woke us up.
			casi = int(casei)
			cas = k
			caseSuccess = sglist.success
			if sglist.releasetime > 0 {
				caseReleaseTime = sglist.releasetime
			}
		} else {
			c = k.c
			if int(casei) < nsends {
				c.sendq.dequeueSudoG(sglist)
			} else {
				c.recvq.dequeueSudoG(sglist)
			}
		}
		sgnext = sglist.waitlink
		sglist.waitlink = nil
		releaseSudog(sglist)
		sglist = sgnext
	}

	if cas == nil {
		throw("selectgo: bad wakeup")
	}

	c = cas.c
	//
	// c 表示唤醒此协程的case
	// caseSuccess 保存者sudog.success 字段的值。
	// casi 表示c在cases数组中的索引。
	//

	if debugSelect {
		print("wait-return: cas0=", cas0, " c=", c, " cas=", cas, " send=", casi < nsends, "\n")
	}

	if casi < nsends {
		// 唤醒协程的case操作为通道写操作
		if !caseSuccess {
			// 因通道关闭而让改通道写操作唤醒完成。
			// 跳到 sclose，先解所有关联通道的锁，再触发panic。
			goto sclose
		}
	} else {
		// 唤醒协程的case操作为通道接收操作
		// recvOk，表示接收到的是否因为通道关闭而赋值的零值。
		recvOK = caseSuccess
	}

	if raceenabled {
		if casi < nsends {
			raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
		} else if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, casePC(casi), chanrecvpc)
		}
	}
	if msanenabled {
		if casi < nsends {
			msanread(cas.elem, c.elemtype.size)
		} else if cas.elem != nil {
			msanwrite(cas.elem, c.elemtype.size)
		}
	}
	if asanenabled {
		if casi < nsends {
			asanread(cas.elem, c.elemtype.size)
		} else if cas.elem != nil {
			asanwrite(cas.elem, c.elemtype.size)
		}
	}

	selunlock(scases, lockorder)
	goto retc

bufrecv:
	// can receive from buffer
	if raceenabled {
		if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, casePC(casi), chanrecvpc)
		}
		racenotify(c, c.recvx, nil)
	}
	if msanenabled && cas.elem != nil {
		msanwrite(cas.elem, c.elemtype.size)
	}
	if asanenabled && cas.elem != nil {
		asanwrite(cas.elem, c.elemtype.size)
	}
	recvOK = true
	qp = chanbuf(c, c.recvx)
	if cas.elem != nil {
		typedmemmove(c.elemtype, cas.elem, qp)
	}
	typedmemclr(c.elemtype, qp)
	c.recvx++
	if c.recvx == c.dataqsiz {
		c.recvx = 0
	}
	c.qcount--
	selunlock(scases, lockorder)
	goto retc

bufsend:
	// can send to buffer
	if raceenabled {
		racenotify(c, c.sendx, nil)
		raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	if asanenabled {
		asanread(cas.elem, c.elemtype.size)
	}
	typedmemmove(c.elemtype, chanbuf(c, c.sendx), cas.elem)
	c.sendx++
	if c.sendx == c.dataqsiz {
		c.sendx = 0
	}
	c.qcount++
	selunlock(scases, lockorder)
	goto retc

recv:
	// can receive from sleeping sender (sg)
	recv(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncrecv: cas0=", cas0, " c=", c, "\n")
	}
	recvOK = true
	goto retc

rclose:
	// read at end of closed channel
	selunlock(scases, lockorder)
	recvOK = false
	if cas.elem != nil {
		typedmemclr(c.elemtype, cas.elem)
	}
	if raceenabled {
		raceacquire(c.raceaddr())
	}
	goto retc

send:
	// can send to a sleeping receiver (sg)
	if raceenabled {
		raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	if asanenabled {
		asanread(cas.elem, c.elemtype.size)
	}
	send(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncsend: cas0=", cas0, " c=", c, "\n")
	}
	goto retc

retc:
	// select 阻塞被唤醒时的返回路径之一
	// 只有2个路径，这个是路径1。
	if caseReleaseTime > 0 {
		blockevent(caseReleaseTime-t0, 1)
	}
	return casi, recvOK

sclose:
	// send on closed channel
	// select 阻塞被唤醒时的返回路径之一
	// 只有2个路径，这个是路径2。
	// 阻塞在写通道操作上且因为通道关闭而唤醒。
	selunlock(scases, lockorder)
	panic(plainError("send on closed channel"))
}

func (c *hchan) sortkey() uintptr {
	return uintptr(unsafe.Pointer(c))
}

// A runtimeSelect is a single case passed to rselect.
// This must match ../reflect/value.go:/runtimeSelect
type runtimeSelect struct {
	dir selectDir
	typ unsafe.Pointer // channel type (not used here)
	ch  *hchan         // channel
	val unsafe.Pointer // ptr to data (SendDir) or ptr to receive buffer (RecvDir)
}

// These values must match ../reflect/value.go:/SelectDir.
type selectDir int

const (
	_             selectDir = iota
	selectSend              // case Chan <- Send
	selectRecv              // case <-Chan:
	selectDefault           // default
)

//go:linkname reflect_rselect reflect.rselect
func reflect_rselect(cases []runtimeSelect) (int, bool) {
	if len(cases) == 0 {
		block()
	}
	sel := make([]scase, len(cases))
	orig := make([]int, len(cases))
	nsends, nrecvs := 0, 0
	dflt := -1
	for i, rc := range cases {
		var j int
		switch rc.dir {
		case selectDefault:
			dflt = i
			continue
		case selectSend:
			j = nsends
			nsends++
		case selectRecv:
			nrecvs++
			j = len(cases) - nrecvs
		}

		sel[j] = scase{c: rc.ch, elem: rc.val}
		orig[j] = i
	}

	// Only a default case.
	if nsends+nrecvs == 0 {
		return dflt, false
	}

	// Compact sel and orig if necessary.
	if nsends+nrecvs < len(cases) {
		copy(sel[nsends:], sel[len(cases)-nrecvs:])
		copy(orig[nsends:], orig[len(cases)-nrecvs:])
	}

	order := make([]uint16, 2*(nsends+nrecvs))
	var pc0 *uintptr
	if raceenabled {
		pcs := make([]uintptr, nsends+nrecvs)
		for i := range pcs {
			selectsetpc(&pcs[i])
		}
		pc0 = &pcs[0]
	}

	chosen, recvOK := selectgo(&sel[0], &order[0], pc0, nsends, nrecvs, dflt == -1)

	// Translate chosen back to caller's ordering.
	if chosen < 0 {
		chosen = dflt
	} else {
		chosen = orig[chosen]
	}
	return chosen, recvOK
}
// 将sudog从通道的sendq或者recvq链表中移除
func (q *waitq) dequeueSudoG(sgp *sudog) {
	x := sgp.prev
	y := sgp.next
	if x != nil {
		if y != nil {
			// middle of queue
			x.next = y
			y.prev = x
			sgp.next = nil
			sgp.prev = nil
			return
		}
		// end of queue
		x.next = nil
		q.last = x
		sgp.prev = nil
		return
	}
	if y != nil {
		// start of queue
		y.prev = nil
		q.first = y
		sgp.next = nil
		return
	}

	// x==y==nil. Either sgp is the only element in the queue,
	// or it has already been removed. Use q.first to disambiguate.
	if q.first == sgp {
		q.first = nil
		q.last = nil
	}
}
