package runtime

import "runtime/internal/sys"
import "sync/atomic"

var NewObj = newobject
type Eface = eface
type Etype = _type
type UncommonType = uncommontype
//var GetCallerSP = getcallersp
var GetCallerP = getcallerpc
var Shouldhelp atomic.Int64
var Par Parr
type Parr struct{arr [32]uintptr;i int}
func (p *Parr)GetArr()[32]uintptr{
	return p.arr
}
func (p *Parr)has(u uintptr)bool{
	for i:=0;i<p.i;i++ {
		if p.arr[i]==u{
			return true
		}
	}
	return false
}

func (p *Parr)add(u uintptr){
	println(p.i)
	p.arr[p.i]=u
	p.i++
}

func GetCallerSP()uintptr{
	return getcallersp()
}
func GetCallerPC()uintptr{
	return getcallerpc()
}
func Info(){
	/*
	println("heapArenaBitmapWords",heapArenaBitmapWords)
	println("logHeapArenaBytes",logHeapArenaBytes)
	println("heapArenaBYtes",heapArenaBytes)
	println("heapAddrBits",heapAddrBits)
	println("heapArenaWords",heapArenaWords)
	println("summaryL0Bits",summaryL0Bits)
	println("arenaBaseOffset",uintptr(arenaBaseOffset))
	println("maxOffAddr",maxOffAddr.a)
	 */
	println(gcController.dedicatedMarkWorkersNeeded.Load())
	println(gcController.fractionalUtilizationGoal)
}

func TflagsInfo(t *_type)(info map[string]bool){
	info=map[string]bool{
		"tflagNamed": t.tflag&tflagNamed!=0,
		"tflagExtraStar" : t.tflag&tflagNamed!=0,
		 "tflagRegularMemory": t.tflag&tflagRegularMemory!=0,
		 "tflagUncommon":t.tflag&tflagUncommon!=0,
	}
	return info
}
func UncommonTypeInfo(t *_type)*UncommonType{
	return  t.uncommon()
}
var Inheap  = inheap
var OneBtes64 = sys.OnesCount64