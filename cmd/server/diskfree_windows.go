//go:build windows

// diskfree_windows.go 启动自检的磁盘剩余空间探测（Windows）。
// 标准库 syscall 未导出 GetDiskFreeSpaceEx，用 kernel32 延迟加载调用（不引入 x/sys 依赖）。
package main

import (
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// diskFreeBytes 返回 path 所在卷的**调用者可用**剩余字节数（GetDiskFreeSpaceEx 的
// lpFreeBytesAvailableToCaller 语义：考虑磁盘配额，与实际可写量一致）。
func diskFreeBytes(path string) (uint64, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	p, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		return 0, err
	}
	var freeToCaller, total, totalFree uint64
	r, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, callErr
	}
	return freeToCaller, nil
}
