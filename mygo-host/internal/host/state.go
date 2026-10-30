// Package host wires the Logseq Electron contract onto the mygo runtime.
package host

import (
	"encoding/json"
	"fmt"
	"github.com/egoist/mygo"
	"logseq/mygo-host/internal/lifecycle"
	"logseq/mygo-host/internal/manager"
	"os"
	"path/filepath"
	"sync"
)

// Config carries the resolved launch options of the host.
type Config struct {
	// ResourcesDir is the directory holding static app assets
	// (index.html, js/, icons/) and the db-worker-node.js daemon script.
	ResourcesDir string
	// DotDir is ~/.logseq (plugins root lives under it).
	DotDir string
	// GraphsDir is the DB graphs directory (~/logseq/graphs).
	GraphsDir string
	// NodeBin is the node executable used to run the daemon script.
	NodeBin string
	// ExpectedRevision is the build revision the daemon must report ("" disables the check).
	ExpectedRevision string
	// DevTools enables DevTools on created windows.
	DevTools bool
	// EntryURL is the main window URL (lsp://logseq.com/index.html).
	EntryURL string
}

// winState tracks per-window graph assignment.
type winState struct {
	graphDir  string // resolved graph directory ("" until setCurrentGraph)
	graphName string // canonical repo name ("" until setCurrentGraph)
}

// Host is the singleton bound to the mygo runtime; its exported methods are
// callable from the renderer through window.mygo.call.
type Host struct {
	cfg Config

	mu              sync.Mutex
	windows         map[int]*winState
	allWindows      map[int]*windowHandle
	pendingRedirect *pendingRedirect
	quitDirty       bool
	proxyOptions    map[string]any
	aborts          map[string]func()

	mgr    *manager.Manager
	locale string

	storageOnce sync.Once
	storage     *lifecycle.Storage
	storageErr  error
}

// pendingRedirect carries a deeplink redirect for a graph that had no open
// window when the link arrived; the first graphReady of a window on that
// graph consumes it (Electron :window/once-graph-ready, url.cljs:55).
type pendingRedirect struct {
	graphName string // canonical repo the redirect targets
	payload   map[string]any
}

func (h *Host) setPendingRedirect(graphName string, payload map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pendingRedirect = &pendingRedirect{graphName: graphName, payload: payload}
}

// takePendingRedirect returns and clears the pending redirect, mirroring
// Electron's single :window/once-graph-ready slot (cleared by the first
// graphReady regardless of which graph reports ready).
func (h *Host) takePendingRedirect() *pendingRedirect {
	h.mu.Lock()
	defer h.mu.Unlock()
	pending := h.pendingRedirect
	h.pendingRedirect = nil
	return pending
}

// windowHandle pairs a mygo window with its lifecycle metadata.
type windowHandle struct {
	id      int
	win     *mygoWindow
	graph   string // canonical repo passed at creation ("" for the main window)
	closed  bool
	onReady []func()
}

func newHost(cfg Config) *Host {
	return &Host{
		cfg:        cfg,
		windows:    map[int]*winState{},
		allWindows: map[int]*windowHandle{},
		aborts:     map[string]func(){},
	}
}

// DotRoot returns ~/.logseq.
func (h *Host) DotRoot() string { return h.cfg.DotDir }

// PluginsRoot returns ~/.logseq/plugins.
func (h *Host) PluginsRoot() string { return filepath.Join(h.cfg.DotDir, "plugins") }

// ResourcesDir returns the static assets directory.
func (h *Host) ResourcesDir() string { return h.cfg.ResourcesDir }

// userDataDir resolves the per-user config directory (mygo PathUserData).
func userDataDir() string {
	dir, err := mygoApp().Path(mygo.PathUserData)
	if err != nil || dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Library", "Application Support", "Logseq")
	}
	return dir
}

// configPath is the userAppCfgs JSON store (renderer-opaque in milestone 1).
func (h *Host) configPath() string { return filepath.Join(userDataDir(), "configs.json") }

func (h *Host) readConfig() map[string]any {
	out := map[string]any{}
	data, err := os.ReadFile(h.configPath())
	if err == nil {
		var parsed map[string]any
		if json.Unmarshal(data, &parsed) == nil && parsed != nil {
			out = parsed
		}
	}
	return out
}

func (h *Host) writeConfig(cfg map[string]any) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(userDataDir(), 0o755); err != nil {
		return err
	}
	tmp := h.configPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, h.configPath())
}

// trackWindow registers a window handle.
func (h *Host) trackWindow(hd *windowHandle) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.allWindows[hd.id] = hd
	h.windows[hd.id] = &winState{}
}

// dropWindow removes a closed window.
func (h *Host) dropWindow(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.allWindows, id)
	delete(h.windows, id)
}

// windowByID returns the handle for a window id.
func (h *Host) windowByID(id int) *windowHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.allWindows[id]
}

// allHandles snapshots the live window handles.
func (h *Host) allHandles() []*windowHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*windowHandle, 0, len(h.allWindows))
	for _, hd := range h.allWindows {
		out = append(out, hd)
	}
	return out
}

// setWindowGraph records the graph a window works on.
func (h *Host) setWindowGraph(id int, graphDir, graphName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.windows[id]; ok {
		st.graphDir = graphDir
		st.graphName = graphName
	}
}

// windowGraph returns the recorded graph dir/name for a window.
func (h *Host) windowGraph(id int) (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.windows[id]; ok {
		return st.graphDir, st.graphName
	}
	return "", ""
}

// countWindows returns the number of alive windows.
func (h *Host) countWindows() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.allWindows)
}

// countOtherWindows counts live windows on a graph dir other than `exceptID`.
func (h *Host) countOtherWindows(graphDir string, exceptID int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for id, st := range h.windows {
		if id != exceptID && st.graphDir != "" && st.graphDir == graphDir {
			n++
		}
	}
	return n
}

// countWindowsOnGraph counts live windows on a graph dir.
func (h *Host) countWindowsOnGraph(graphDir string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, st := range h.windows {
		if st.graphDir != "" && st.graphDir == graphDir {
			n++
		}
	}
	return n
}

// findWindowOnGraph returns the id of a live window on a graph dir.
func (h *Host) findWindowOnGraph(graphDir string) (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, st := range h.windows {
		if st.graphDir == graphDir {
			return id, true
		}
	}
	return 0, false
}

// setQuitDirty records the renderer's dirty state before quitting.
func (h *Host) setQuitDirty(dirty bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.quitDirty = dirty
}

// registerAbort stores a cancel func for an HTTP request id.
func (h *Host) registerAbort(id string, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.aborts[id] = cancel
}

// popAbort removes and returns the cancel func for a request id.
func (h *Host) popAbort(id string) (func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cancel, ok := h.aborts[id]
	delete(h.aborts, id)
	return cancel, ok
}

// boolArg returns args[i] as bool (default when missing/invalid).
func boolArg(args []any, i int, def bool) bool {
	if len(args) <= i {
		return def
	}
	if b, ok := args[i].(bool); ok {
		return b
	}
	return def
}

// maybeNumberArg converts args[i] to float64 when possible.
func maybeNumberArg(v any) map[string]any {
	if f, ok := v.(float64); ok {
		return map[string]any{"v": f}
	}
	return nil
}

// maybeMap narrows any to map[string]any (nil otherwise).
func maybeMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// intOf coerces common numeric types to int64.
func intOf(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func stringArg(args []any, i int) string {
	if i < len(args) {
		if s, ok := args[i].(string); ok {
			return s
		}
	}
	return ""
}

func argOf(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}
	return nil
}

func mapArg(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func stringOf(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func boolOf(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

func numberOf(m map[string]any, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

func fmtErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", err.Error())
}
