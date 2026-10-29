package lifecycle

import (
	"errors"
	"syscall"
)

// pidExistsErr returns the raw error from signaling the pid.
func pidExistsErr(pid int) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	return killErr(pid)
}

func isESRCH(err error) bool { return errors.Is(err, syscall.ESRCH) }

func isEPERM(err error) bool { return errors.Is(err, syscall.EPERM) }

func asLifecycleError(err error, out **Error) bool {
	return errors.As(err, out)
}
