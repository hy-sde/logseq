package host

// Port of src/electron/electron/embedding_server.cljs: manages the local
// python embedding sidecar (venv + embedding_server.py) and publishes its
// endpoint via LOGSEQ_EMBEDDINGS_URL so later-spawned db-worker daemons
// inherit it (lifecycle ops.go copies os.Environ()).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	embeddingRuntimeDirName = "embedding-server"
	embeddingVenvName       = ".venv"
	embeddingDepsStampName  = "deps-v2.ok"
	embeddingLogFileName    = "embedding-server.log"
	embeddingURLEnv         = "LOGSEQ_EMBEDDINGS_URL"
	embeddingPythonEnv      = "LOGSEQ_EMBEDDINGS_PYTHON"
	embeddingModelEnv       = "LOGSEQ_EMBEDDING_MODEL"
	embeddingModelDefault   = "all-MiniLM-L6-v2"
	embeddingReadyTimeout   = 120 * time.Second
	embeddingReadyPoll      = 100 * time.Millisecond
)

// embeddingDependencies mirrors the cljs dependency vector; the deps stamp
// records them so reinstalls only happen when the list changes.
var embeddingDependencies = []string{"sentence-transformers", "httpx[socks]"}

type embeddingState struct {
	mu      sync.Mutex
	started bool
	proc    *exec.Cmd
	url     string
}

func (h *Host) embedding() *embeddingState { return &h.embeddingState }

// embeddingRuntimeDir mirrors <user-data>/embedding-server; the host uses
// the logseq dot dir as its user-data equivalent.
func (h *Host) embeddingRuntimeDir() string {
	dotDir := h.cfg.DotDir
	if dotDir == "" {
		dotDir = h.DotRoot()
	}
	return filepath.Join(dotDir, embeddingRuntimeDirName)
}

// embeddingSidecarDir mirrors the cljs sidecar-dir: packaged apps keep the
// python script next to static under Contents/Resources; dev runs resolve
// <repo>/sidecar because resourcesDir is <repo>/static.
func (h *Host) embeddingSidecarDir() string {
	if h.cfg.ResourcesDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(h.cfg.ResourcesDir), "sidecar")
}

func embeddingHealthURL(hostAddr string, port int) string {
	return fmt.Sprintf("http://%s:%d/healthz", hostAddr, port)
}

func embeddingEndpointURL(hostAddr string, port int) string {
	return fmt.Sprintf("http://%s:%d/v1/embeddings", hostAddr, port)
}

// findFreePort mirrors find-port!: bind :0 on the host, read the port, close.
func findFreePort(hostAddr string) (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(hostAddr, "0"))
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("embedding: unexpected listener address %T", l.Addr())
	}
	return addr.Port, nil
}

// StartEmbeddingServer is the setup!/start! port: macOS-only, idempotent,
// fully async — failures are logged and never block boot.
func (h *Host) StartEmbeddingServer() {
	if runtime.GOOS != "darwin" {
		return
	}
	st := h.embedding()
	st.mu.Lock()
	if st.started {
		st.mu.Unlock()
		return
	}
	st.started = true
	st.mu.Unlock()

	go func() {
		if err := h.runEmbeddingServer(); err != nil {
			log.Printf("[host] embedding-server setup failed: %v", err)
			st.mu.Lock()
			st.started = false
			h.stopEmbeddingServerLocked()
			st.mu.Unlock()
		}
	}()
}

func (h *Host) runEmbeddingServer() error {
	sidecarRoot := h.embeddingSidecarDir()
	scriptPath := filepath.Join(sidecarRoot, "embedding_server.py")
	if _, err := os.Stat(scriptPath); err != nil {
		return fmt.Errorf("embedding script missing: %w", err)
	}

	hostAddr := "127.0.0.1"
	port, err := findFreePort(hostAddr)
	if err != nil {
		return fmt.Errorf("find free port: %w", err)
	}
	// reserve-endpoint!: publish (unready) before the slow venv setup so
	// concurrent ensure calls can observe the endpoint.
	st := h.embedding()
	st.mu.Lock()
	st.url = embeddingEndpointURL(hostAddr, port)
	url := st.url
	st.mu.Unlock()
	log.Printf("[host] embedding-server endpoint reserved: %s", url)

	runtimeDir := h.embeddingRuntimeDir()
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return fmt.Errorf("runtime dir: %w", err)
	}

	venvPython, err := h.installEmbeddingRuntime(runtimeDir)
	if err != nil {
		return err
	}

	logFile := filepath.Join(runtimeDir, embeddingLogFileName)
	modelID := strings.TrimSpace(os.Getenv(embeddingModelEnv))
	if modelID == "" {
		modelID = embeddingModelDefault
	}

	proc, err := h.spawnEmbeddingServer(venvPython, scriptPath, sidecarRoot, hostAddr, port, modelID, logFile)
	if err != nil {
		return fmt.Errorf("spawn server: %w", err)
	}
	st.mu.Lock()
	st.proc = proc
	st.mu.Unlock()

	healthURL := embeddingHealthURL(hostAddr, port)
	if err := waitEmbeddingReady(healthURL); err != nil {
		return fmt.Errorf("health check %s: %w", healthURL, err)
	}

	// publish-endpoint!: export into the host process env — worker daemons
	// spawn with os.Environ() and inherit it.
	if err := os.Setenv(embeddingURLEnv, url); err != nil {
		return fmt.Errorf("publish env: %w", err)
	}
	log.Printf("[host] embedding-server ready: %s", url)
	return nil
}

// installEmbeddingRuntime ports install-runtime!: reuse a usable venv,
// recreate it when broken, pip-install deps when the stamp is missing.
func (h *Host) installEmbeddingRuntime(runtimeDir string) (string, error) {
	venvDir := filepath.Join(runtimeDir, embeddingVenvName)
	candidates := []string{
		filepath.Join(venvDir, "bin", "python"),
		filepath.Join(venvDir, "bin", "python3"),
	}

	venvPython := ""
	for _, candidate := range candidates {
		if !fileExists(candidate) {
			continue
		}
		if err := runCommandQuiet(candidate, []string{"-c", "import sys"}, runtimeDir); err == nil {
			venvPython = candidate
			break
		}
	}

	depsStamp := filepath.Join(runtimeDir, embeddingDepsStampName)
	needsVenv := venvPython == ""
	needsDeps := needsVenv || !fileExists(depsStamp)

	if needsVenv {
		_ = os.RemoveAll(venvDir)
		pythonCommand := strings.TrimSpace(os.Getenv(embeddingPythonEnv))
		if pythonCommand == "" {
			pythonCommand = "python3"
		}
		if err := runCommandQuiet(pythonCommand, []string{"-m", "venv", embeddingVenvName}, runtimeDir); err != nil {
			return "", fmt.Errorf("venv creation: %w", err)
		}
		for _, candidate := range candidates {
			if fileExists(candidate) && runCommandQuiet(candidate, []string{"-c", "import sys"}, runtimeDir) == nil {
				venvPython = candidate
				break
			}
		}
		if venvPython == "" {
			return "", errors.New("embedding server virtualenv Python is missing")
		}
	}

	if needsDeps {
		if err := runCommandQuiet(venvPython, append([]string{"-m", "pip", "install"}, embeddingDependencies...), runtimeDir); err != nil {
			return "", fmt.Errorf("pip install: %w", err)
		}
		stamp := strings.Join(embeddingDependencies, "\n") + "\n"
		if err := os.WriteFile(depsStamp, []byte(stamp), 0o644); err != nil {
			return "", fmt.Errorf("write deps stamp: %w", err)
		}
	}
	return venvPython, nil
}

func (h *Host) spawnEmbeddingServer(venvPython, scriptPath, sidecarRoot, hostAddr string, port int, modelID, logFile string) (*exec.Cmd, error) {
	cmd := exec.Command(venvPython, scriptPath,
		"--host", hostAddr,
		"--port", fmt.Sprintf("%d", port),
		"--model", modelID,
		"--log-file", logFile)
	cmd.Dir = sidecarRoot
	// Own process group so Stop kills the python child tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go logStream("[host] embedding-server", stdout)
	go logStream("[host] embedding-server", stderr)
	go func() {
		err := cmd.Wait()
		st := h.embedding()
		st.mu.Lock()
		if st.proc == cmd {
			st.proc = nil
			st.url = ""
		}
		st.mu.Unlock()
		_ = os.Unsetenv(embeddingURLEnv)
		log.Printf("[host] embedding-server exited: %v", err)
	}()
	return cmd, nil
}

// waitEmbeddingReady ports wait-ready!: poll /healthz until 2xx or deadline.
func waitEmbeddingReady(healthURL string) error {
	deadline := time.Now().Add(embeddingReadyTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, err := client.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("health check failed: status %d", resp.StatusCode)
		}
		time.Sleep(embeddingReadyPoll)
	}
}

// StopEmbeddingServer ports stop!.
func (h *Host) StopEmbeddingServer() {
	st := h.embedding()
	st.mu.Lock()
	st.started = false
	h.stopEmbeddingServerLocked()
	st.mu.Unlock()
}

// stopEmbeddingServerLocked kills the sidecar process group; callers hold
// the embedding mutex.
func (h *Host) stopEmbeddingServerLocked() {
	st := h.embedding()
	if st.proc != nil {
		proc := st.proc
		st.proc = nil
		st.url = ""
		if proc.Process != nil {
			_ = syscall.Kill(-proc.Process.Pid, syscall.SIGTERM)
			go func() {
				done := make(chan struct{})
				go func() { _ = proc.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					_ = syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
				}
			}()
		}
	}
	_ = os.Unsetenv(embeddingURLEnv)
}

func runCommandQuiet(name string, args []string, dir string) error {
	// Upstream imposes no timeout; the cap is generous enough for multi-GB
	// sentence-transformers (torch) installs on slow networks.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.Run()
}

func logStream(prefix string, r interface{ Read([]byte) (int, error) }) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			log.Printf("%s: %s", prefix, strings.TrimSpace(string(buf[:n])))
		}
		if err != nil {
			return
		}
	}
}
