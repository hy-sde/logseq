//go:build unix

package lifecycle

import (
	"errors"
	"os"
	"syscall"
)

// flockLock holds an exclusive advisory lock on an open file for the
// lifetime of the handle (flock LOCK_EX, non-blocking acquire).
type flockLock struct{ f *os.File }

func tryFlock(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

func unlockFlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// pidExists mirrors lifecycle pidExists: signal(pid, 0).
func pidExists(pid int) bool {
err := killErr(pid)
return err == nil || errors.Is(err, syscall.EPERM)
}

// killErr exposes the raw signal error for pid-status classification.
func killErr(pid int) error {
	return syscall.Kill(pid, 0)
}

func signalProcess(pid int, sig syscall.Signal) error {
	if pid == os.Getpid() {
		return errSelfSignal
	}
	if !pidExists(pid) {
		return nil
	}
	return syscall.Kill(pid, sig)
}
