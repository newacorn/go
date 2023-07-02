package runtime


var NewObj = newobject
type Eface = eface
type Etype = _type
type UncommonType = uncommontype
//var GetCallerSP = getcallersp
var GetCallerP = getcallerpc
func GetCallerSP()uintptr{
	return getcallersp()
}
func GetCallerPC()uintptr{
	return getcallerpc()
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