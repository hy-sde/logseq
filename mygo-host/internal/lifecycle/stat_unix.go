//go:build unix

package lifecycle

import (
	"os"
	"syscall"
)

// sysDev/sysIno expose the directory identity fields for sameDirectory.
func sysDev(info os.FileInfo) uint64 { return uint64(info.Sys().(*syscall.Stat_t).Dev) }
func sysIno(info os.FileInfo) uint64 { return uint64(info.Sys().(*syscall.Stat_t).Ino) }

// detachedProcAttr detaches children owned by non-electron owners (the CLI
// spawns daemons that outlive the process).
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// releaseChild releases OS resources for an intentionally long-lived child.
func releaseChild(proc *os.Process) {}
