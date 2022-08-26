// created by cgo -cdefs and then converted to Go
// cgo -cdefs defs_linux.go defs1_linux.go

//go:build linux
// +build linux

package netpoll

type epollevent struct {
	events uint32
	data   [8]byte // unaligned uintptr
}
