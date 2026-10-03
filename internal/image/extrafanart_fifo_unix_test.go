//go:build unix

package image

import "syscall"

func init() { makeFifoHook = func(p string) error { return syscall.Mkfifo(p, 0o600) } }
