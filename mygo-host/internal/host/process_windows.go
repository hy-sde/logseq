//go:build windows

package host

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32Host     = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcessH = kernel32Host.NewProc("OpenProcess")
	procTerminateH   = kernel32Host.NewProc("TerminateProcess")
	procGetExitCodeH = kernel32Host.NewProc("GetExitCodeProcess")
)

const (
	processTerminateHost  = 0x0001 // PROCESS_TERMINATE
	processQueryLimitedH  = 0x0400 // PROCESS_QUERY_LIMITED_INFORMATION
	stillActiveHost       = 0x103  // STILL_ACTIVE
	errorAccessDeniedHost = syscall.Errno(5)
)

// sysProcAttrOwnGroup: Windows has no Unix process groups; the graceful
// stop path remains the sidecar's HTTP /v1/shutdown.
func sysProcAttrOwnGroup() *syscall.SysProcAttr {
	return nil
}

// killProcessGroup terminates the sidecar process hard (the child tree is
// left to exit on its own; Job-object tree kills are out of scope here).
func killProcessGroup(pid int, sig syscall.Signal) error {
	if sig != syscall.SIGKILL {
		return fmt.Errorf("signal %v unsupported on windows", sig)
	}
	h, _, _ := procOpenProcessH.Call(processTerminateHost, 0, uintptr(pid))
	if h == 0 {
		return nil // already gone
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	procTerminateH.Call(h, 1)
	return nil
}

// groupAlive probes the sidecar process; false once the handle cannot be
// opened or the process has exited.
func groupAlive(pid int) bool {
	h, _, err := procOpenProcessH.Call(processQueryLimitedH, 0, uintptr(pid))
	if h == 0 {
		return err == errorAccessDeniedHost
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var code uint32
	r1, _, _ := procGetExitCodeH.Call(h, uintptr(unsafe.Pointer(&code)))
	return r1 != 0 && code == stillActiveHost
}
