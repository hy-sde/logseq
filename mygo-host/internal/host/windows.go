package host

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/egoist/mygo"
	"logseq/mygo-host/internal/daemon"
	"logseq/mygo-host/internal/manager"
	"logseq/mygo-host/internal/lifecycle"
)

// wID returns the numeric id of a caller window (0 when unknown).
func wID(w *mygoWindow) int {
	if w == nil {
		return 0
	}
	return w.ID()
}

// notifyErrorArgs emits a "notification" {type:"error"} event to the given
// window (nil → all windows). Mirrors handler.cljs notify.
func (h *Host) notifyErrorArgs(w *mygoWindow, payload string, i18nKey string, i18nArgs []any) {
	m := map[string]any{"type": "error", "payload": payload}
	if i18nKey != "" {
		m["i18n-key"] = i18nKey
	}
	if len(i18nArgs) > 0 {
		m["i18n-args"] = i18nArgs
	}
	if w != nil {
		evNotification.Emit(w, m)
		return
	}
	h.emitToAll(evNotification, m)
}

// ---- user configs (configs.json in user data dir) ----

func (h *Host) setConfigItem(key string, value any) error {
	cfg := h.readConfig()
	cfg[key] = value
	return h.writeConfig(cfg)
}

func (h *Host) getConfigItem(key string) any {
	cfg := h.readConfig()
	v, ok := cfg[key]
	if !ok {
		return nil
	}
	return v
}

// ---- clipboard ----

// clipboardFilePath reads the first file path on the clipboard
// (mac public.file-url style); milestone 1 uses macOS osascript.
func clipboardFilePath() (any, error) {
	out, err := exec.Command("osascript", "-e",
		"the clipboard as «class furl»").Output()
	if err != nil || len(out) == 0 {
		return "", nil
	}
	s := strings.TrimSpace(string(out))
	s = strings.TrimPrefix(s, "«class furl»")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "file://")
	return s, nil
}

// clipboardData returns clipboard content for a format string
// ("text/plain" → text, anything else nil for milestone 1).
func clipboardData(format string) (any, error) {
	switch {
	case strings.Contains(format, "text"):
		out, err := exec.Command("pbpaste").Output()
		if err != nil {
			return "", nil
		}
		return string(out), nil
	default:
		return "", nil
	}
}

// ---- window creation ----

type createWindowOpts struct {
	Graph string // canonical repo for extra windows ("" = main window)
}

// createWindow builds a mygo window with the logseq preload shim and the
// lsp:// entry URL, registering it in the host window table.
func (h *Host) createWindow(opts createWindowOpts) (*mygoWindow, error) {
	entryURL := h.cfg.EntryURL
	if opts.Graph != "" {
		entryURL = strings.TrimSuffix(entryURL, "/index.html") + "/#/?graph=" + opts.Graph
	}
	wopts := mygo.WindowOptions{
		Title:           "Logseq",
		URL:             entryURL,
		Width:           1280,
		Height:          800,
		MinWidth:        940,
		MinHeight:       460,
		StateKey:        "logseq-main",
		BackgroundColor: "#ffffff",
		AutoHideMenuBar: runtime.GOOS != "darwin",
		Page: mygo.PageOptions{
			PreloadScript: preloadJS,
			DevTools:      devToolsOption(),
		},
	}
	// LOGSEQ_WIN_CHROME=1 opts into hidden-inset titlebar chrome. DISABLED by
	// default: mygo v0.7.1 on macOS paints a BLANK window when either
	// Hidden:true or TitleBarStyle:TitleBarHiddenInset is set (window stays
	// non-key and the WKWebView layer never composites; bisected 2026-10-29,
	// CapturePage shows content fine → framework bug, not ours).
	if os.Getenv("LOGSEQ_WIN_CHROME") == "1" {
		wopts.Hidden = true
		wopts.TitleBarStyle = mygo.TitleBarHiddenInset
		wopts.TrafficLightPosition = &mygo.Point{X: 16, Y: 16}
	}
	win := mygo.NewWindow(wopts)
	h.trackWindowHandle(win, opts.Graph)
	h.attachDebugProbe(win)
	if wopts.Hidden {
		// mygo's Show() orders the window front but does NOT activate it
		// (internal/darwin/window.go: "Showing it does not activate"); a
		// non-key window never paints WKWebView content on macOS.
		win.OnReadyToShow(func() {
			win.Show()
			win.Focus()
		})
	}
	// Plain executables do not come to the front on their own; a non-key
	// window may never paint WKWebView content on macOS.
	mygo.App.Focus()
	return win, nil
}

func devToolsOption() mygo.DevTools {
	if mygo.IsDev() {
		return mygo.DevToolsAuto
	}
	return mygo.DevToolsDisabled
}

// trackWindowHandle registers a window and wires close-time bookkeeping.
func (h *Host) trackWindowHandle(win *mygoWindow, graph string) {
	hd := &windowHandle{id: win.ID(), win: win, graph: graph}
	h.trackWindow(hd)
	win.OnClosed(func() {
		h.dropWindow(hd.id)
		h.ReleaseWindow(hd.id)
	})
}

// ReleaseWindow detaches a window from any graph runtime (manager glue).
func (h *Host) ReleaseWindow(windowID int) {
	if h.mgr != nil {
		h.mgr.ReleaseWindow(windowID)
	}
}

// ---- manager glue ----

// initManager constructs the db-worker manager with host notifications.
func (h *Host) initManager(cfg daemon.Config) {
	h.mgr = manager.NewManager(manager.Config{
		RootDir:          cfg.RootDir,
		GraphsDir:        cfg.GraphsDir,
		Storage:          cfg.Storage,
		ScriptPath:       cfg.ScriptPath,
		NodeBin:          cfg.NodeBin,
		ExpectedRevision: cfg.ExpectedRevision,
	}, h.onGraphLifecycle)
}

func (h *Host) onGraphLifecycle(repo string, payload map[string]any) {
	payload["repo"] = repo
	h.emitToAll(evGraphLifecycle, payload)
}



// stopAllWorkers is called on quit.
func (h *Host) stopAllWorkers() {
	if h.mgr != nil {
		h.mgr.StopAll()
	}
}

// ensureStorage resolves the lifecycle storage once for graph commands.
func (h *Host) ensureStorage() (*lifecycle.Storage, error) {
	h.storageOnce.Do(func() {
		cfg := daemon.Config{GraphsDir: h.cfg.GraphsDir}
		st, err := daemon.ResolveStorage(cfg)
		h.storage = st
		h.storageErr = err
	})
	return h.storage, h.storageErr
}

// ---- misc ----

func marshalIndent(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

var _ = fmt.Sprintf
