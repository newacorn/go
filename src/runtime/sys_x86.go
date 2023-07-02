// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || 386

package runtime

import (
	"internal/goarch"
	"unsafe"
)

// gostartcall
// 参数：fn 函数指针，goroutine运行的第一条指令
// 		ctxt 闭包指针，其包裹了参数fn
// 将 buf.sp向下移动一个指针大小，在上面存储buf.pc
// 更新 buf.sp=buf.sp-1，buf.pc=fn
// 当G关联函数执行完毕后会执行开始的buf.pc，相当于
// goroutine的return address
// adjust Gobuf as if it executed a call to fn with context ctxt
// and then stopped before the first instruction in fn.
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	sp := buf.sp
	sp -= goarch.PtrSize
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc
	buf.sp = sp
	buf.pc = uintptr(fn)
	buf.ctxt = ctxt
}
