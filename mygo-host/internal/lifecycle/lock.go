package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"logseq/mygo-host/internal/lqgraph"
)

var errSelfSignal = errors.New("cannot stop the calling process")

func sha256Hex(s string) string { return lqgraph.SHA256Hex(s) }

// GraphName re-exported from lqgraph.
func GraphName(repo string) (string, error) { return lqgraph.GraphName(repo) }

func EncodeGraphName(repo string) string {
	name, err := lqgraph.GraphName(repo)
	if err != nil {
		return ""
	}
	enc, _ := lqgraph.EncodeGraphName(name)
	return enc
}

// canonicalRoot mirrors lifecycle canonicalRoot: expand "~/", realpath.
func canonicalRoot(root string) string { return lqgraph.CanonicalizePath(root) }

// ExpandHomeTilde expands a leading "~/".
func ExpandHomeTilde(p string) string { return lqgraph.ExpandHome(p) }

// runtimeLocksRoot mirrors ownership.cjs runtimeRoot().
func runtimeLocksRoot() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Logseq", "runtime-locks")
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(local, "Logseq", "runtime-locks")
	default:
		return filepath.Join(home, ".local", "state", "Logseq", "runtime-locks")
	}
}

// ownershipLockPath mirrors ownership.cjs ownershipPath(ctx): the lock lives
// beside the Node lock.sqlite, keyed by the canonical graph directory.
func ownershipLockPath(ctx *GraphCtx) string {
	canonical := ctx.GraphDir
	if real, err := filepath.EvalSymlinks(ctx.GraphDir); err == nil {
		canonical = real
	} else {
		// Graph dir may not exist yet (createGraph): resolve the parent.
		parent := filepath.Dir(ctx.GraphDir)
		if real, err := filepath.EvalSymlinks(parent); err == nil {
			canonical = filepath.Join(real, filepath.Base(ctx.GraphDir))
		}
	}
	return filepath.Join(runtimeLocksRoot(), sha256Hex(canonical), "lock.lock")
}

// ownership is an exclusive flock handle over the graph ownership file.
type ownership struct{ f *os.File }

// acquireOwnership mirrors ownership.cjs acquireOwnership: a non-blocking
// exclusive lock; contention is repo-locked.
func acquireOwnership(ctx *GraphCtx) (*ownership, error) {
	lockFile := ownershipLockPath(ctx)
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if !tryFlock(f) {
		f.Close()
		return nil, coded("repo-locked", "Graph ownership is locked: %s", ctx.GraphDir)
	}
	return &ownership{f: f}, nil
}

func (o *ownership) release() {
	if o == nil || o.f == nil {
		return
	}
	unlockFlock(o.f)
	o.f.Close()
	o.f = nil
}

// ownershipAvailable probes ownership without holding it.
func ownershipAvailable(ctx *GraphCtx) bool {
	handle, err := acquireOwnership(ctx)
	if err != nil {
		return false
	}
	handle.release()
	return true
}

// lease is the lifecycle mutual-exclusion handle (flock on lease.lock).
type lease struct {
	f     *os.File
	ctx   *GraphCtx
	op    string
	owner string
}

// acquireLease mirrors acquireLease: wait up to 30s in 25ms steps, then
// stamp {owner: {id, pid, operation}} into state.json; release() removes it.
func acquireLease(ctx *GraphCtx, operation string) (*lease, error) {
	lockFile := filepath.Join(ctx.Dir, "lease.lock")
	deadline := time.Now().Add(30 * time.Second)
	var f *os.File
	for {
		var err error
		f, err = os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
		if err == nil {
			if tryFlock(f) {
				break
			}
			f.Close()
		}
		if time.Now().After(deadline) {
			if err != nil {
				return nil, err
			}
			return nil, fail("Timed out acquiring lifecycle lease for %s", ctx.Repo)
		}
		time.Sleep(25 * time.Millisecond)
	}
	l := &lease{f: f, ctx: ctx, op: operation, owner: id()}
	current, err := State(ctx)
	if err != nil {
		unlockFlock(f)
		f.Close()
		return nil, err
	}
	current["owner"] = map[string]any{"id": l.owner, "pid": os.Getpid(), "operation": operation}
	if err := writeJSON(ctx.StateFile, current); err != nil {
		unlockFlock(f)
		f.Close()
		return nil, err
	}
	return l, nil
}

// release mirrors the JS release closure: verify ownership stamp, remove it.
func (l *lease) release() {
	if l == nil || l.f == nil {
		return
	}
	defer func() {
		unlockFlock(l.f)
		l.f.Close()
		l.f = nil
	}()
	latest, err := readJSON(l.ctx.StateFile)
	if err != nil || latest == nil {
		return
	}
	owner, _ := latest["owner"].(map[string]any)
	if owner == nil || owner["id"] != l.owner {
		// The JS impl fails here; in Go we only log (a stale lease stamp
		// after a crashed operation must not wedge the file forever).
		logLifecycleEvent("lease-ownership-changed", l.ctx.Repo, "")
		return
	}
	delete(latest, "owner")
	_ = writeJSON(l.ctx.StateFile, latest)
}

// withLease mirrors lifecycle withLease.
func withLease(ctx *GraphCtx, operation string, action func(current map[string]any) (any, error)) (any, error) {
	l, err := acquireLease(ctx, operation)
	if err != nil {
		return nil, err
	}
	defer l.release()
	current, err := State(ctx)
	if err != nil {
		return nil, err
	}
	return action(current)
}

// logLifecycleEvent writes the same stderr marker line the JS module emits.
func logLifecycleEvent(event, repo, stage string) {
	line := map[string]any{"event": event, "repo": repo}
	if stage != "" {
		line["stage"] = stage
	}
	data, _ := json.Marshal(line)
	fmt.Fprintf(os.Stderr, "[graph-lifecycle] %s\n", data)
}

var _ = syscall.SIGTERM
var _ = strings.TrimSpace
