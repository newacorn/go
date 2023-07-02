// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Runtime type representation.

package runtime

import (
	"internal/abi"
	"unsafe"
)

// tflag is documented in reflect/type.go.
//
// tflag values must be kept in sync with copies in:
//
//	cmd/compile/internal/reflectdata/reflect.go
//	cmd/link/internal/ld/decodesym.go
//	reflect/type.go
//	internal/reflectlite/type.go
type tflag uint8

const (
	tflagUncommon      tflag = 1 << 0
	tflagExtraStar     tflag = 1 << 1
	tflagNamed         tflag = 1 << 2
	tflagRegularMemory tflag = 1 << 3 // equal and hash can treat values of this type as a single region of t.size bytes
)

// _type 提供了适用于所有类型的最基本的描述，对于一些更复杂的类型，例如符合类型slice和map等，run
// time中分别定义了 maptype slicetype 等对应的结构。例如 slicetype 就是一个由一个用来描述类型本身
// 的 _type 结构和一个指向元素类型的指针组成。
// Needs to be in sync with ../cmd/link/internal/ld/decodesym.go:/^func.commonsize,
// ../cmd/compile/internal/reflectdata/reflect.go:/^func.dcommontype and
// ../reflect/type.go:/^type.rtype.
// ../internal/reflectlite/type.go:/^type.rtype.
type _type struct {
	// 此类型的数据需要占用多少字节的存储空间。被 newobject / mallocgc 所需要。
	size       uintptr
	// 表示数据的前多少字节包含指针，用来应用写屏障时优化范围大小，例如某个stuct类型在
	// 64位平台占32字节，但只有第一个字段是指针类型，这个值就是8，剩下的24字节就不需要写
	// 屏障了。GC进行位图标记的时候，也会用到该字段。
	ptrdata    uintptr // size of memory prefix holding all pointers
	// 当前类型的哈希值，runtime基于这个值构建类型映射表，加速类型比较和查找。
	hash       uint32
	// 额外的类型标识，目前由4个独立的二进制位组合而成。 tflagUncommon 表明这种类型元数据结构后面
	// 有个紧邻的 uncommontype 结构， uncommontype 主要在自定义类型定义方法集时用到。 tflagExtraStar
	// 表示类型的名称字符串有个前缀 *， 因为对于程序中的大多数类型T而言，*T也同样存在，复用同一个名称字符串
	// 能够节省空间。 tflagNamed 表示类型有名称。 tflagRegularMemory 表示相等比较和哈希函数可以把该
	// 类型的数据当成内存中的单块区间来处理，即类型没有间接部分。
	tflag      tflag
	// 表示当前类型变量的对齐边界
	align      uint8
	// 表示当前类型的struct字段的对齐边界
	fieldAlign uint8
	// 表示当前类型所属的分类，目前Go语言的reflect包定义了26种有效分类。
	kind       uint8
	// function for comparing objects of this type
	// (ptr to object A, ptr to object B) -> ==?
	// 用来比较两个当前类型的变量是否相等。
	equal func(unsafe.Pointer, unsafe.Pointer) bool
	// gcdata stores the GC type data for the garbage collector.
	// If the KindGCProg bit is set in kind, gcdata is a GC program.
	// Otherwise it is a ptrmask bitmap. See mbitmap.go for details.
	// 和垃圾回收相关，GC扫描和写屏障用来追踪指针。
	gcdata    *byte
	// 偏移，通过str可以找到当前类型的名称等文本信息。
	str       nameOff
	// 偏移，假设当期类型为T，通过它可以找到类型*T的类型元数据。
	ptrToThis typeOff
}

func (t *_type) string() string {
	s := t.nameOff(t.str).name()
	if t.tflag&tflagExtraStar != 0 {
		return s[1:]
	}
	return s
}
// uncommon 方法，对于一个自定义类型可以通过此方法得到一个指向 uncommontype 结构的指针
// ,也就是说编译器会为自定义类型生成一个 uncommontype 结构。
func (t *_type) uncommon() *uncommontype {
	if t.tflag&tflagUncommon == 0 {
		return nil
	}
	switch t.kind & kindMask {
	case kindStruct:
		type u struct {
			structtype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindPtr:
		type u struct {
			ptrtype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindFunc:
		type u struct {
			functype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindSlice:
		type u struct {
			slicetype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindArray:
		type u struct {
			arraytype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindChan:
		type u struct {
			chantype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindMap:
		type u struct {
			maptype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindInterface:
		type u struct {
			interfacetype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	default:
		type u struct {
			_type
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	}
}

func (t *_type) name() string {
	if t.tflag&tflagNamed == 0 {
		return ""
	}
	s := t.string()
	i := len(s) - 1
	sqBrackets := 0
	for i >= 0 && (s[i] != '.' || sqBrackets != 0) {
		switch s[i] {
		case ']':
			sqBrackets++
		case '[':
			sqBrackets--
		}
		i--
	}
	return s[i+1:]
}

// pkgpath returns the path of the package where t was defined, if
// available. This is not the same as the reflect package's PkgPath
// method, in that it returns the package path for struct and interface
// types, not just named types.
func (t *_type) pkgpath() string {
	if u := t.uncommon(); u != nil {
		return t.nameOff(u.pkgpath).name()
	}
	switch t.kind & kindMask {
	case kindStruct:
		st := (*structtype)(unsafe.Pointer(t))
		return st.pkgPath.name()
	case kindInterface:
		it := (*interfacetype)(unsafe.Pointer(t))
		return it.pkgpath.name()
	}
	return ""
}

// reflectOffs holds type offsets defined at run time by the reflect package.
//
// When a type is defined at run time, its *rtype data lives on the heap.
// There are a wide range of possible addresses the heap may use, that
// may not be representable as a 32-bit offset. Moreover the GC may
// one day start moving heap memory, in which case there is no stable
// offset that can be defined.
//
// To provide stable offsets, we add pin *rtype objects in a global map
// and treat the offset as an identifier. We use negative offsets that
// do not overlap with any compile-time module offsets.
//
// Entries are created by reflect.addReflectOff.
var reflectOffs struct {
	lock mutex
	next int32
	m    map[int32]unsafe.Pointer
	minv map[unsafe.Pointer]int32
}

func reflectOffsLock() {
	lock(&reflectOffs.lock)
	if raceenabled {
		raceacquire(unsafe.Pointer(&reflectOffs.lock))
	}
}

func reflectOffsUnlock() {
	if raceenabled {
		racerelease(unsafe.Pointer(&reflectOffs.lock))
	}
	unlock(&reflectOffs.lock)
}

func resolveNameOff(ptrInModule unsafe.Pointer, off nameOff) name {
	if off == 0 {
		return name{}
	}
	base := uintptr(ptrInModule)
	for md := &firstmoduledata; md != nil; md = md.next {
		if base >= md.types && base < md.etypes {
			res := md.types + uintptr(off)
			if res > md.etypes {
				println("runtime: nameOff", hex(off), "out of range", hex(md.types), "-", hex(md.etypes))
				throw("runtime: name offset out of range")
			}
			return name{(*byte)(unsafe.Pointer(res))}
		}
	}

	// No module found. see if it is a run time name.
	reflectOffsLock()
	res, found := reflectOffs.m[int32(off)]
	reflectOffsUnlock()
	if !found {
		println("runtime: nameOff", hex(off), "base", hex(base), "not in ranges:")
		for next := &firstmoduledata; next != nil; next = next.next {
			println("\ttypes", hex(next.types), "etypes", hex(next.etypes))
		}
		throw("runtime: name offset base pointer out of range")
	}
	return name{(*byte)(res)}
}

func (t *_type) nameOff(off nameOff) name {
	return resolveNameOff(unsafe.Pointer(t), off)
}

func resolveTypeOff(ptrInModule unsafe.Pointer, off typeOff) *_type {
	if off == 0 || off == -1 {
		// -1 is the sentinel value for unreachable code.
		// See cmd/link/internal/ld/data.go:relocsym.
		return nil
	}
	// 根据base确定在哪个模块类型区间里查找
	base := uintptr(ptrInModule)
	var md *moduledata
	for next := &firstmoduledata; next != nil; next = next.next {
		if base >= next.types && base < next.etypes {
			md = next
			break
		}
	}
	if md == nil {
		// 如果未找到模块，说明此类型是运行时通过反射创建的, 要在reflectOffs中查找。
		reflectOffsLock()
		res := reflectOffs.m[int32(off)]
		reflectOffsUnlock()
		if res == nil {
			println("runtime: typeOff", hex(off), "base", hex(base), "not in ranges:")
			for next := &firstmoduledata; next != nil; next = next.next {
				println("\ttypes", hex(next.types), "etypes", hex(next.etypes))
			}
			throw("runtime: type offset base pointer out of range")
		}
		return (*_type)(res)
	}
	// 先在模块的 typemap字段中查找
	if t := md.typemap[off]; t != nil {
		return t
	}
	// 但是第一个模块的typemap 字段为nil。
	// 所以再尝试通过偏移来查找。
	// see: runtime.typelinksinit
	res := md.types + uintptr(off)
	if res > md.etypes {
		println("runtime: typeOff", hex(off), "out of range", hex(md.types), "-", hex(md.etypes))
		throw("runtime: type offset out of range")
	}
	return (*_type)(unsafe.Pointer(res))
}

func (t *_type) typeOff(off typeOff) *_type {
	return resolveTypeOff(unsafe.Pointer(t), off)
}

func (t *_type) textOff(off textOff) unsafe.Pointer {
	if off == -1 {
		// -1 is the sentinel value for unreachable code.
		// See cmd/link/internal/ld/data.go:relocsym.
		return unsafe.Pointer(abi.FuncPCABIInternal(unreachableMethod))
	}
	base := uintptr(unsafe.Pointer(t))
	var md *moduledata
	for next := &firstmoduledata; next != nil; next = next.next {
		if base >= next.types && base < next.etypes {
			md = next
			break
		}
	}
	if md == nil {
		reflectOffsLock()
		res := reflectOffs.m[int32(off)]
		reflectOffsUnlock()
		if res == nil {
			println("runtime: textOff", hex(off), "base", hex(base), "not in ranges:")
			for next := &firstmoduledata; next != nil; next = next.next {
				println("\ttypes", hex(next.types), "etypes", hex(next.etypes))
			}
			throw("runtime: text offset base pointer out of range")
		}
		return res
	}
	res := md.textAddr(uint32(off))
	return unsafe.Pointer(res)
}

func (t *functype) in() []*_type {
	// See funcType in reflect/type.go for details on data layout.
	uadd := uintptr(unsafe.Sizeof(functype{}))
	if t.typ.tflag&tflagUncommon != 0 {
		uadd += unsafe.Sizeof(uncommontype{})
	}
	return (*[1 << 20]*_type)(add(unsafe.Pointer(t), uadd))[:t.inCount]
}

func (t *functype) out() []*_type {
	// See funcType in reflect/type.go for details on data layout.
	uadd := uintptr(unsafe.Sizeof(functype{}))
	if t.typ.tflag&tflagUncommon != 0 {
		uadd += unsafe.Sizeof(uncommontype{})
	}
	outCount := t.outCount & (1<<15 - 1)
	return (*[1 << 20]*_type)(add(unsafe.Pointer(t), uadd))[t.inCount : t.inCount+outCount]
}

func (t *functype) dotdotdot() bool {
	return t.outCount&(1<<15) != 0
}

type nameOff int32
type typeOff int32
type textOff int32

// method 为类型方法集（数组）的元素类型，按name进行升序排列。
// 指针接收者方法的 ifn 和 tfn 的值是一样的。
type method struct {
	// name 通过这个偏移可以找到方法名称字符串。
	// nameOff 相对于具体类型 _type 的起始地址偏移量。
	// 可以通过 reflect.(*rtype).nameOff(t *_type, name nameOff) 返回的 reflect.name 类型的值
	// 然后通过其 name 方法来获取字符串名称。
	name nameOff
	// mtyp 偏移除是方法的类型元数据，进一步可以已找到参数和返回值的类型元数据
	// 可以通过 reflect.(*rtype).typeOff(t *_type, mtyp typeOff) 返回的 reflect.(*rtype) 类型的值
	// 再通过其 String 方法可以获得方法类型的字符串表示。
	mtyp typeOff
	// ifn 供接口调用的方法地址。 ifn 的接收者类型一定是指针。
	// 因为接口中 data 字段总是保存了动态值的地址，所以可以直接调用这个方法。
	// 可以通过 reflect.(*rtype).textOff(t *_type, inf textOff) 返回的方法的指针
	ifn  textOff
	// tfn 是正常方法地址，tfn的接收者类型跟源代码中的实现一致。
	// 可以通过 reflect.(*rtype).textOff(t *_type, inf textOff) 返回的方法的指针
	tfn  textOff
}

type uncommontype struct {
	// pkgpath 定义该类型的包名称
	pkgpath nameOff
	// mcount 该类型共有多少个方法
	mcount  uint16 // nummber of methods
	// xcount 该类型有多少个方法被导出
	xcount  uint16 // numbetr of exported methods
	// moff 是个偏移值，那里就是方法方法集的元数据，也就是一组 method 结构构成的数组。
	moff    uint32 // offset from this uncommontype to [mcount]method
	_       uint32 // unused
}

// imethod 接口的方法对应的数据结构
// 必自定义类型的方法 method 结构少了方法地址，只包含方法名和类型
// 元数据的偏移。这些偏移的实际类型为int32，与指针的作用一样，但是在
// 64位平台比使用指针节省一半空间。
type imethod struct {
	name nameOff
	// ityp 方法元信息的偏移，以 ityp 为起点，可以找到方法的参数（包括
	// 返回值）列表，以及每个参数的类型信息，也就是说这个 ityp 是方法的
	// 原型信息。
	ityp typeOff
}

// 接口类型的元数据信息对应的数据结构
type interfacetype struct {
	typ     _type
	// pkgpath 表示接口类型被定义在哪个包
	pkgpath name
	// mhdr 是接口声明的方法列表
	mhdr    []imethod
}

type maptype struct {
	typ    _type
	key    *_type
	elem   *_type
	bucket *_type // internal type representing a hash bucket
	// function for hashing keys (ptr to key, seed) -> hash
	hasher     func(unsafe.Pointer, uintptr) uintptr
	keysize    uint8  // size of key slot
	elemsize   uint8  // size of elem slot
	bucketsize uint16 // size of bucket
	flags      uint32
}

// Note: flag values must match those used in the TMAP case
// in ../cmd/compile/internal/reflectdata/reflect.go:writeType.
func (mt *maptype) indirectkey() bool { // store ptr to key instead of key itself
	return mt.flags&1 != 0
}
func (mt *maptype) indirectelem() bool { // store ptr to elem instead of elem itself
	return mt.flags&2 != 0
}
func (mt *maptype) reflexivekey() bool { // true if k==k for all keys
	return mt.flags&4 != 0
}
func (mt *maptype) needkeyupdate() bool { // true if we need to update key on an overwrite
	return mt.flags&8 != 0
}
func (mt *maptype) hashMightPanic() bool { // true if hash function might panic
	return mt.flags&16 != 0
}

type arraytype struct {
	typ   _type
	elem  *_type
	slice *_type
	len   uintptr
}

type chantype struct {
	typ  _type
	elem *_type
	dir  uintptr
}

type slicetype struct {
	typ  _type
	elem *_type
}

type functype struct {
	typ      _type
	inCount  uint16
	outCount uint16
}

type ptrtype struct {
	typ  _type
	elem *_type
}

type structfield struct {
	name   name
	typ    *_type
	offset uintptr
}

type structtype struct {
	typ     _type
	pkgPath name
	fields  []structfield
}

// name is an encoded type name with optional extra data.
// See reflect/type.go for details.
type name struct {
	bytes *byte
}

func (n name) data(off int) *byte {
	return (*byte)(add(unsafe.Pointer(n.bytes), uintptr(off)))
}

func (n name) isExported() bool {
	return (*n.bytes)&(1<<0) != 0
}

func (n name) isEmbedded() bool {
	return (*n.bytes)&(1<<3) != 0
}

func (n name) readvarint(off int) (int, int) {
	v := 0
	for i := 0; ; i++ {
		x := *n.data(off + i)
		v += int(x&0x7f) << (7 * i)
		if x&0x80 == 0 {
			return i + 1, v
		}
	}
}

func (n name) name() string {
	if n.bytes == nil {
		return ""
	}
	i, l := n.readvarint(1)
	if l == 0 {
		return ""
	}
	return unsafe.String(n.data(1+i), l)
}

func (n name) tag() string {
	if *n.data(0)&(1<<1) == 0 {
		return ""
	}
	i, l := n.readvarint(1)
	i2, l2 := n.readvarint(1 + i + l)
	return unsafe.String(n.data(1+i+l+i2), l2)
}

func (n name) pkgPath() string {
	if n.bytes == nil || *n.data(0)&(1<<2) == 0 {
		return ""
	}
	i, l := n.readvarint(1)
	off := 1 + i + l
	if *n.data(0)&(1<<1) != 0 {
		i2, l2 := n.readvarint(off)
		off += i2 + l2
	}
	var nameOff nameOff
	copy((*[4]byte)(unsafe.Pointer(&nameOff))[:], (*[4]byte)(unsafe.Pointer(n.data(off)))[:])
	pkgPathName := resolveNameOff(unsafe.Pointer(n.bytes), nameOff)
	return pkgPathName.name()
}

func (n name) isBlank() bool {
	if n.bytes == nil {
		return false
	}
	_, l := n.readvarint(1)
	return l == 1 && *n.data(2) == '_'
}

// typelinksinit 函数返回后，所有模块内部使用的类型信息都可以在其 moduledata.typemap 映射中
// 找到，用的键还是本模块存放类型元数据区段的偏移量(相对于 moduledata.types )，而值_type指针有
// 可能指向的是其他模块的内存元数据区间。 这样就达到了所有模块内部类型元信息的唯一性。
// 这里的模块只的是二进制模块，比如将某个模块构建为插件。
// 不同module在一起构建的认为是一个模块。
// typelinksinit scans the types from extra modules and builds the
// moduledata typemap used to de-duplicate type pointers.
func typelinksinit() {
	// 没有其他二进制模块时（plugin），firstmoduledata.next的值为nil
	if firstmoduledata.next == nil {
		return
	}
	// 用来收集所有模块中的类型信息，用类型的`hash`作为`map`的key，收集的类型元数据
	// _type结构的地址，把hash相同类型的地址放到同一个slice中。
	typehash := make(map[uint32][]*_type, len(firstmoduledata.typelinks))
    // activeModules 函数得到当前活动模块的列表，也就是所有能够正常使用的Go
	// 二进制模块，然后从第二个模块开始向后遍历。
	modules := activeModules()
	prev := modules[0]
	for _, md := range modules[1:] {
		// Collect types from the previous module into typehash.
	collect:
		// tl 为模块内的类型元数据地址相对于moduledata.types 的偏移量。
		for _, tl := range prev.typelinks {
			var t *_type
			if prev.typemap == nil {
				t = (*_type)(unsafe.Pointer(prev.types + uintptr(tl)))
			} else {
				t = prev.typemap[typeOff(tl)]
			}
			// Add to typehash if not seen before.
			tlist := typehash[t.hash]
			for _, tcur := range tlist {
				// 如果模块prev在之前下面的去重遍历中发现了自己模块中的类型与之前模块的重复
				// 就会使用之前的模块类型地址（它们都收集在typehash中）。所以下面 tcur==t
				// 会出现为真的情况。如果在去重遍历中使用了自己的类型地址，这不会出现下面地址
				// 相等的情况。
				if tcur == t {
					continue collect
				}
			}
			// typsehash 这里存储着hash形同但类型地址不同的_type。
			// 因为hash相同不能代表_type是相同的，所以这里都要收集，用于下
			// 一步深度去重做准备。
			typehash[t.hash] = append(tlist, t)
		}

		// 所有模块中除了 firstmoduledata 的 typemap 字段为nil，其它模块都不会为nil。
		if md.typemap == nil {
			// 如果当前模块的 typemap 位nil，就分配一个新的map并填充数据。遍历当前
			// 模块的 typelinks，对于其中所有的类型，先去 typehash 中查找，优先使用
			// typehash 中的类型地址， typehash 中没有的类型才使用当前模块自身包含
			// 的地址，把地址添加到 typemap 中。
			// If any of this module's typelinks match a type from a
			// prior module, prefer that prior type by adding the offset
			// to this module's typemap.
			tm := make(map[typeOff]*_type, len(md.typelinks))
			// pinnedTypemaps 主要避免GC回收掉 typemap，因为模块列表对GC不可见。
			pinnedTypemaps = append(pinnedTypemaps, tm)
			md.typemap = tm
			for _, tl := range md.typelinks {
				t := (*_type)(unsafe.Pointer(md.types + uintptr(tl)))
				// hash 相同只是去重的第一步
				for _, candidate := range typehash[t.hash] {
					seen := map[_typePair]struct{}{}
					// 在哈希相同的情况下，还需深度比较，因为hash会发生碰撞。
					if typesEqual(t, candidate, seen) {
						t = candidate
						break
					}
				}
				md.typemap[typeOff(tl)] = t
			}
		}

		prev = md
	}
}

type _typePair struct {
	t1 *_type
	t2 *_type
}

// typesEqual reports whether two types are equal.
//
// Everywhere in the runtime and reflect packages, it is assumed that
// there is exactly one *_type per Go type, so that pointer equality
// can be used to test if types are equal. There is one place that
// breaks this assumption: buildmode=shared. In this case a type can
// appear as two different pieces of memory. This is hidden from the
// runtime and reflect package by the per-module typemap built in
// typelinksinit. It uses typesEqual to map types from later modules
// back into earlier ones.
//
// Only typelinksinit needs this function.
func typesEqual(t, v *_type, seen map[_typePair]struct{}) bool {
	tp := _typePair{t, v}
	if _, ok := seen[tp]; ok {
		return true
	}

	// mark these types as seen, and thus equivalent which prevents an infinite loop if
	// the two types are identical, but recursively defined and loaded from
	// different modules
	seen[tp] = struct{}{}

	if t == v {
		return true
	}
	kind := t.kind & kindMask
	if kind != v.kind&kindMask {
		return false
	}
	if t.string() != v.string() {
		return false
	}
	ut := t.uncommon()
	uv := v.uncommon()
	if ut != nil || uv != nil {
		if ut == nil || uv == nil {
			return false
		}
		pkgpatht := t.nameOff(ut.pkgpath).name()
		pkgpathv := v.nameOff(uv.pkgpath).name()
		if pkgpatht != pkgpathv {
			return false
		}
	}
	if kindBool <= kind && kind <= kindComplex128 {
		return true
	}
	switch kind {
	case kindString, kindUnsafePointer:
		return true
	case kindArray:
		at := (*arraytype)(unsafe.Pointer(t))
		av := (*arraytype)(unsafe.Pointer(v))
		return typesEqual(at.elem, av.elem, seen) && at.len == av.len
	case kindChan:
		ct := (*chantype)(unsafe.Pointer(t))
		cv := (*chantype)(unsafe.Pointer(v))
		return ct.dir == cv.dir && typesEqual(ct.elem, cv.elem, seen)
	case kindFunc:
		ft := (*functype)(unsafe.Pointer(t))
		fv := (*functype)(unsafe.Pointer(v))
		if ft.outCount != fv.outCount || ft.inCount != fv.inCount {
			return false
		}
		tin, vin := ft.in(), fv.in()
		for i := 0; i < len(tin); i++ {
			if !typesEqual(tin[i], vin[i], seen) {
				return false
			}
		}
		tout, vout := ft.out(), fv.out()
		for i := 0; i < len(tout); i++ {
			if !typesEqual(tout[i], vout[i], seen) {
				return false
			}
		}
		return true
	case kindInterface:
		it := (*interfacetype)(unsafe.Pointer(t))
		iv := (*interfacetype)(unsafe.Pointer(v))
		if it.pkgpath.name() != iv.pkgpath.name() {
			return false
		}
		if len(it.mhdr) != len(iv.mhdr) {
			return false
		}
		for i := range it.mhdr {
			tm := &it.mhdr[i]
			vm := &iv.mhdr[i]
			// Note the mhdr array can be relocated from
			// another module. See #17724.
			tname := resolveNameOff(unsafe.Pointer(tm), tm.name)
			vname := resolveNameOff(unsafe.Pointer(vm), vm.name)
			if tname.name() != vname.name() {
				return false
			}
			if tname.pkgPath() != vname.pkgPath() {
				return false
			}
			tityp := resolveTypeOff(unsafe.Pointer(tm), tm.ityp)
			vityp := resolveTypeOff(unsafe.Pointer(vm), vm.ityp)
			if !typesEqual(tityp, vityp, seen) {
				return false
			}
		}
		return true
	case kindMap:
		mt := (*maptype)(unsafe.Pointer(t))
		mv := (*maptype)(unsafe.Pointer(v))
		return typesEqual(mt.key, mv.key, seen) && typesEqual(mt.elem, mv.elem, seen)
	case kindPtr:
		pt := (*ptrtype)(unsafe.Pointer(t))
		pv := (*ptrtype)(unsafe.Pointer(v))
		return typesEqual(pt.elem, pv.elem, seen)
	case kindSlice:
		st := (*slicetype)(unsafe.Pointer(t))
		sv := (*slicetype)(unsafe.Pointer(v))
		return typesEqual(st.elem, sv.elem, seen)
	case kindStruct:
		st := (*structtype)(unsafe.Pointer(t))
		sv := (*structtype)(unsafe.Pointer(v))
		if len(st.fields) != len(sv.fields) {
			return false
		}
		if st.pkgPath.name() != sv.pkgPath.name() {
			return false
		}
		for i := range st.fields {
			tf := &st.fields[i]
			vf := &sv.fields[i]
			if tf.name.name() != vf.name.name() {
				return false
			}
			if !typesEqual(tf.typ, vf.typ, seen) {
				return false
			}
			if tf.name.tag() != vf.name.tag() {
				return false
			}
			if tf.offset != vf.offset {
				return false
			}
			if tf.name.isEmbedded() != vf.name.isEmbedded() {
				return false
			}
		}
		return true
	default:
		println("runtime: impossible type kind", kind)
		throw("runtime: impossible type kind")
		return false
	}
}
