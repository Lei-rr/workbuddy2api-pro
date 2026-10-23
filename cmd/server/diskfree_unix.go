//go:build unix

// diskfree_unix.go 启动自检的磁盘剩余空间探测（Linux/macOS/BSD 等 unix）。
package main

import "syscall"

// diskFreeBytes 返回 path 所在文件系统的**非特权用户可用**剩余字节数
// （Statfs.Bavail：扣除 root 保留块，与实际可写量一致）。
func diskFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
