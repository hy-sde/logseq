package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestFindFreePort(t *testing.T) {
	port, err := findFreePort("127.0.0.1")
	if err != nil {
		t.Fatalf("findFreePort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port out of range: %d", port)
	}
}

func TestStopEmbeddingServerKillsProcessGroup(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-group kill test needs unix")
	}
	h := newHost(Config{})
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	st := h.embedding()
	st.mu.Lock()
	st.proc = cmd
	h.stopEmbeddingServerLocked()
	st.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for {
		// Signal 0 probes liveness; ESRCH means the group is gone. Do not
		// read cmd.ProcessState here: the stop watcher's proc.Wait() writes
		// it concurrently and -race flags the unsynchronized read.
		if err := syscall.Kill(-cmd.Process.Pid, 0); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("embedding server process group still alive after stop")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := os.LookupEnv(embeddingURLEnv); ok {
		t.Errorf("%s should be unset after stop", embeddingURLEnv)
	}
}

func TestEmbeddingDirs(t *testing.T) {
	h := newHost(Config{DotDir: "/tmp/dot", ResourcesDir: "/repo/static"})
	if got := h.embeddingRuntimeDir(); got != filepath.Join("/tmp/dot", embeddingRuntimeDirName) {
		t.Errorf("runtime dir = %q", got)
	}
	if got := h.embeddingSidecarDir(); got != "/repo/sidecar" {
		t.Errorf("sidecar dir = %q", got)
	}
}
