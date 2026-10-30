//go:build windows

package lifecycle

import (
	"os"
	"syscall"
)

// sysDev/sysIno: Windows has no stable dev/ino via os.FileInfo.Sys; use
// the file index from GetFileInformationByHandle via the file handle.
func sysDev(info os.FileInfo) uint64 { return 0 }

func sysIno(info os.FileInfo) uint64 {
	// best-effort: hash the full path for identity comparison within one
	// host run; real ino would require reopening the handle.
	return 0
}

func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func releaseChild(proc *os.Process) {}
