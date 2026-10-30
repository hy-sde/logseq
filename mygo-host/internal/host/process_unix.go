//go:build unix

package host

import "syscall"

// sysProcAttrOwnGroup puts the child into its own process group so Stop
// can signal the whole python child tree.
func sysProcAttrOwnGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the child's process group (negated pid).
func killProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

// groupAlive probes the child's process group; false once every member is
// gone (ESRCH).
func groupAlive(pid int) bool {
	return syscall.Kill(-pid, 0) == nil
}
