//go:build !windows

package main

import "syscall"

// detachedProcAttr starts a child in its own session, so it survives the
// terminal that started it (px relay --daemon).
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
