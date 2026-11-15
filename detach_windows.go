//go:build windows

package main

import "syscall"

// detachedProcAttr starts a child detached from the console (px relay
// --daemon): DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200, HideWindow: true}
}
