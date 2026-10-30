//go:build windows

package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	processQueryLimitedInformation = 0x1000
	processTerminate               = 0x0001
	stillActive                    = 259
	lockfileExclusiveLock          = 0x2
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess  = kernel32.NewProc("OpenProcess")
	procGetExitCode  = kernel32.NewProc("GetExitCodeProcess")
	procTerminate    = kernel32.NewProc("TerminateProcess")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// flockLock on Windows is an exclusive LockFileEx over the whole file.
func tryFlock(f *os.File) bool {
	ol := syscall.Overlapped{}
	r1, _, _ := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock, 0,
		0xFFFFFFFF, 0x7FFFFFFF, uintptr(unsafe.Pointer(&ol)))
	return r1 != 0
}

func unlockFlock(f *os.File) {
	ol := syscall.Overlapped{}
	procUnlockFileEx.Call(f.Fd(), 0, 0xFFFFFFFF, 0x7FFFFFFF, uintptr(unsafe.Pointer(&ol)))
}

// pidExists mirrors lifecycle pidExists on Windows.
func pidExists(pid int) bool {
	err := killErr(pid)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// killErr classifies pid liveness for pid-status: not-found when the
// handle cannot be opened, alive when it can.
func killErr(pid int) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	h, _, err := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		// ERROR_ACCESS_DENIED means it exists but we cannot see it
		if err == syscall.ERROR_ACCESS_DENIED {
			return syscall.EPERM
		}
		return syscall.ESRCH
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var code uint32
	r1, _, _ := procGetExitCode.Call(h, uintptr(unsafe.Pointer(&code)))
	if r1 != 0 && code == stillActive {
		return nil
	}
	return syscall.ESRCH
}

// signalProcess: graceful signals do not exist on Windows; the daemon's
// HTTP /v1/shutdown is the graceful path. SIGKILL terminates hard.
func signalProcess(pid int, sig syscall.Signal) error {
	if pid == os.Getpid() {
		return errSelfSignal
	}
	if !pidExists(pid) {
		return nil
	}
	if sig != syscall.SIGKILL {
		return fmt.Errorf("signal %v unsupported on windows", sig)
	}
	h, _, _ := procOpenProcess.Call(processTerminate, 0, uintptr(pid))
	if h == 0 {
		return nil // already gone
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	procTerminate.Call(h, 1)
	return nil
}
