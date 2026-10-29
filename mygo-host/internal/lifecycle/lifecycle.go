// Package lifecycle ports deps/graph-lifecycle/index.cjs (protocol
// "sqlite-v1") to Go for the mygo desktop host.
//
// Deviations from the Node implementation (documented in README):
//   - The lease and graph ownership files use flock (lease.lock,
//     lock.lock) instead of sqlite (lease.sqlite, lock.sqlite). Go cannot
//     take a sqlite BEGIN IMMEDIATE with the stdlib, and it must never
//     corrupt the Node-owned sqlite files. Cross-engine exclusion between
//     the Go host and a Node CLI during lifecycle operations is therefore
//     not serialized; Node processes still serialize among themselves.
//   - Legacy (pre-sqlite-v1) artifacts fail fast with
//     "Legacy ownership unresolved" instead of being shut down.
//   - observe() polls state.json every 250ms instead of fs.watch.
package lifecycle

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Protocol is the worker ownership protocol version.
const Protocol = "sqlite-v1"

const readyPollMaxElapsed = 2000 // ms; one poll is 50ms sleep + <=1s request

// Error carries a lifecycle failure code, mirroring the JS {code} property.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(format string, args ...any) *Error {
	return &Error{Code: "server-stop-failed", Message: fmt.Sprintf(format, args...)}
}

func coded(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsCode reports whether err is a lifecycle error with the given code.
func IsCode(err error, code string) bool {
	var le *Error
	return errors.As(err, &le) && le.Code == code
}

// Code returns the lifecycle error code, or "".
func Code(err error) string {
	var le *Error
	if errors.As(err, &le) {
		return le.Code
	}
	return ""
}

func id() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", s[0:8], s[8:12], s[12:16], s[16:20], s[20:32])
}

// Storage is the canonical triple returned by ResolveStorage.
type Storage struct {
	Root         string `json:"root"`
	GraphsDir    string `json:"graphsDir"`
	LifecycleDir string `json:"lifecycleDir"`
}

// Context addresses one graph's lifecycle state.
type GraphCtx struct {
	Root         string
	GraphsDir    string
	LifecycleDir string
	Repo         string // graph name without the logseq_db_ prefix
	Dir          string // <lifecycleDir>/<encoded repo>
	GraphDir     string // <graphsDir>/<encoded repo>
	StateFile    string
}

// ResolveStorage ports lifecycle resolveStorage.
func ResolveStorage(root, graphsDir string) (*Storage, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	cRoot := canonicalRoot(root)
	if graphsDir == "" {
		return nil, coded("missing-graphs-dir", "graphs-dir is required")
	}
	if err := os.MkdirAll(graphsDir, 0o755); err != nil {
		return nil, err
	}
	cGraphs := canonicalRoot(graphsDir)
	lifecycleDir := filepath.Join(filepath.Dir(cGraphs), ".graph-lifecycle", sha256Hex(cGraphs))
	if err := os.MkdirAll(lifecycleDir, 0o755); err != nil {
		return nil, err
	}
	return &Storage{Root: cRoot, GraphsDir: cGraphs, LifecycleDir: lifecycleDir}, nil
}

// Context ports lifecycle context(storage, repo).
func Context(storage *Storage, repo string) (*GraphCtx, error) {
	if storage.Root == "" || storage.GraphsDir == "" || storage.LifecycleDir == "" {
		return nil, fail("Canonical storage context is required")
	}
	name, err := GraphName(repo)
	if err != nil {
		return nil, err
	}
	enc := EncodeGraphName(name)
	dir := filepath.Join(storage.LifecycleDir, enc)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &GraphCtx{
		Root: storage.Root, GraphsDir: storage.GraphsDir, LifecycleDir: storage.LifecycleDir,
		Repo: name, Dir: dir,
		GraphDir:  filepath.Join(storage.GraphsDir, enc),
		StateFile: filepath.Join(dir, "state.json"),
	}, nil
}

// Snapshot reads the raw state.json for a repo (nil when absent).
func Snapshot(storage *Storage, repo string) (map[string]any, error) {
	name, err := GraphName(repo)
	if err != nil {
		return nil, err
	}
	return readJSON(filepath.Join(storage.LifecycleDir, EncodeGraphName(name), "state.json"))
}

func sameStorage(ctx *GraphCtx, value map[string]any) bool {
	if value == nil {
		return false
	}
	graphsDir, _ := value["graphsDir"].(string)
	lifecycleDir, _ := value["lifecycleDir"].(string)
	if graphsDir == "" || lifecycleDir == "" {
		return false
	}
	return canonicalRoot(graphsDir) == ctx.GraphsDir && canonicalRoot(lifecycleDir) == ctx.LifecycleDir
}

// readJSON mirrors lifecycle readJSON: nil (not error) on ENOENT.
func readJSON(file string) (map[string]any, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// writeJSON mirrors lifecycle writeJSON: write temp then rename.
func writeJSON(file string, value any) error {
	tmp := file + "." + id() + ".tmp"
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, file)
}

// State mirrors lifecycle state(): init + validate state.json.
func State(ctx *GraphCtx) (map[string]any, error) {
	value, err := readJSON(ctx.StateFile)
	if err != nil {
		return nil, err
	}
	if value == nil {
		if _, statErr := os.Stat(ctx.StateFile); os.IsNotExist(statErr) {
			phase := "absent"
			if _, err := os.Stat(ctx.GraphDir); err == nil {
				phase = "available"
			}
			value = map[string]any{"generation": id(), "phase": phase, "workers": []any{}}
			if err := writeJSON(ctx.StateFile, value); err != nil {
				return nil, err
			}
		}
	}
	if value == nil {
		return nil, fail("Invalid lifecycle state")
	}
	workers, _ := value["workers"].([]any)
	generation, _ := value["generation"].(string)
	phase, _ := value["phase"].(string)
	if value["workers"] != nil && workers == nil {
		return nil, fail("Invalid lifecycle state")
	}
	if generation == "" || phase == "" {
		return nil, fail("Invalid lifecycle state")
	}
	if workers == nil {
		value["workers"] = []any{}
	}
	return value, nil
}

func requireAvailable(ctx *GraphCtx, current map[string]any, generation string) error {
	if generation != "" && generation != current["generation"] {
		return coded("graph-not-exists", "Graph generation changed")
	}
	phase, _ := current["phase"].(string)
	_, statErr := os.Stat(ctx.GraphDir)
	if phase != "available" || statErr != nil {
		return coded("graph-not-exists", "Graph is absent or stopped by deletion")
	}
	return nil
}
