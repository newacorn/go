package runtime

import "runtime/internal/sys"
func GetCallerSp() uintptr {
	v := getcallersp()
	c := getcallerpc()
	println(c)

	return v
}
func GetCurg() *g {
	return getg()
}

func SystemSack(f func()) {
	systemstack(func() {
		f()
	})
}
func NewM(f func()) {
	systemstack(func() {
		newm(f, nil, -1)
	})
}
func Printcreatedby(g *g) {
	printcreatedby(g)
}
func PrintPc(pc uintptr) {
	// Show what created goroutine, except main goroutine (goid 1).
	gp := getg()
	f := findfunc(pc)
	if f.valid() && showframe(f, gp, false, funcID_normal, funcID_normal) && gp.goid != 1 {
		printcreatedby1(f, pc)
	}
}
func Hint(){
	v :=mheap_.arenaHints
	println(&v)
}
func DD(v uint64,m uint)uint64{
	return  fillAligned(v,m)
}
func LeadZeor(x uint64){

	println("leadzero",sys.LeadingZeros64(x))
}
func GCHeapLive(){
	println("gcController.heapLive",gcController.heapLive.Load())
}

type M struct {
	d []uint64
	i int64
}

var Dup = M{d:make([]uint64,1024)}

func DDup(uint642 uint64){
	if Dup.i>1023{
		return
	}
	for _,v:=range Dup.d{
		if uint642==v{
			return
		}
	}
	Dup.d[Dup.i]=uint642
	Dup.i++
}
var nn note
func Note(){
	notewakeup(&nn)
	go func() {
		println(8888)
		notetsleepg(&nn,-1)
		println(111999)
	}()
	notetsleepg(&nn,-1)
	// noteclear(&nn)
	println(88)
}