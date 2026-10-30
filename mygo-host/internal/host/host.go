package host

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/egoist/mygo"
	"logseq/mygo-host/internal/daemon"
	"logseq/mygo-host/internal/lifecycle"
)

// Run wires the whole host together and blocks on the mygo event loop.
func Run() error {
	h := newHost(loadHostConfig())

	// App identity must be set before Run (affects user-data dir, keychain).
	mygoApp().SetName("Logseq")
	if v := os.Getenv("LOGSEQ_VERSION"); v != "" {
		mygoApp().SetVersion(v)
	}

	// Deeplink scheme (logseq://) — register before Run.
	if err := mygoApp().RegisterURLScheme("logseq"); err != nil {
		log.Printf("[host] register logseq scheme: %v", err)
	}

	// Custom schemes: lsp:// serves the app + plugins, assets:// raw files.
	if err := mygo.Protocol.Handle("lsp", &lspHandler{host: h}); err != nil {
		return fmt.Errorf("lsp protocol: %w", err)
	}
	if err := mygo.Protocol.Handle("assets", &assetsHandler{}); err != nil {
		return fmt.Errorf("assets protocol: %w", err)
	}

	// IPC services (bound names = window.mygo.call("Name.Method", ...)).
	mygo.BindAs("LqMain", &LqMain{H: h})
	mygo.BindAs("LqSync", &LqSync{H: h})
	mygo.BindAs("LqChannels", &LqChannels{H: h})

	// Single instance: a second launch forwards argv to this process.
	if !mygoApp().RequestSingleInstanceLock() {
		log.Printf("[host] another instance is running; exiting")
		os.Exit(0)
	}
	mygoApp().OnSecondInstance(func(args []string, workingDir string) {
		h.onSecondInstance(args)
	})

	// Deeplinks: logseq://graph/... etc (url.cljs).
	mygoApp().OnOpenURL(func(rawURL string) {
		h.handleDeeplink(rawURL)
	})

	// App lifecycle.
	mygoApp().OnWindowAllClosed(func() {
		// Electron quits on window-all-closed (all platforms in logseq).
		h.stopAllWorkers()
		mygoApp().Quit()
	})
	mygoApp().OnBeforeQuit(func(e *mygo.QuitEvent) {
		// core.cljs before-quit sets *quitting? and stops db workers; the
		// mygo host additionally confirms when the renderer has not
		// cleared the dirty flag (set-quit-dirty-state).
		if h.isQuitDirty() && !h.confirmQuitDialog() {
			log.Printf("[host] quit prevented by confirm dialog")
			e.PreventDefault()
			return
		}
		log.Printf("[host] quitting (dirty=%v)", h.isQuitDirty())
		h.stopAllWorkers()
	})
	mygoApp().WhenReady(func() {
		h.onReady()
	})

	return mygoApp().Run()
}

// confirmQuitDialog asks before quitting while the renderer's dirty flag
// is set (core.cljs *quit-dirty?). Returns true when the quit proceeds.
func (h *Host) confirmQuitDialog() bool {
	res, err := mygo.Dialog.Message(mygo.MessageOptions{
		Title:        "Logseq",
		Message:      "Quit Logseq?",
		Detail:       "Unsaved edits may be lost.",
		Buttons:      []string{"Quit", "Cancel"},
		CancelButton: 1,
	})
	if err != nil {
		return true // fail open: never trap the user in the app
	}
	return res.Button == 0
}

// loadHostConfig resolves host configuration from flags/env.
func loadHostConfig() Config {
	cfg := Config{}
	cfg.ResourcesDir = envOr("LOGSEQ_RESOURCES", defaultResourcesDir())
	cfg.DotDir = envOr("LOGSEQ_DOT_DIR", dotRootDefault())
	cfg.GraphsDir = daemon.GetDBGraphsDir()
	cfg.NodeBin = envOr("LOGSEQ_DB_WORKER_NODE_BIN", "node")
	cfg.ExpectedRevision = os.Getenv("LOGSEQ_EXPECTED_REVISION")
	cfg.EntryURL = envOr("LOGSEQ_ENTRY_URL", "lsp://logseq.com/index.html")
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultResourcesDir: next to the executable (static/ layout), inside a
// macOS app bundle (Contents/Resources/static, produced by
// scripts/build-app.sh), falling back to the repo's static dir during
// development.
func defaultResourcesDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		local := filepath.Join(dir, "static")
		if dirExists(local) {
			return local
		}
		// .app bundle layout: <app>/Contents/MacOS/<bin>.
		bundled := filepath.Join(dir, "..", "Resources", "static")
		if dirExists(bundled) {
			return filepath.Clean(bundled)
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if dirExists(filepath.Join(wd, "static")) {
			return filepath.Join(wd, "static")
		}
	}
	return "static"
}

func dotRootDefault() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".logseq"
	}
	return filepath.Join(home, ".logseq")
}

// onReady starts the worker manager and opens the first window.
func (h *Host) onReady() {
	// App menu before the first window so the menu bar is set on all
	// platforms (window menus on linux/win apply to created windows).
	h.installAppMenu()

	// Embedding sidecar boots async (venv + pip can take minutes); workers
	// started before it is ready simply miss LOGSEQ_EMBEDDINGS_URL until
	// their next restart, matching the Electron setup! race.
	go h.StartEmbeddingServer()

	// Resolve the daemon script early so failures surface in the log.
	if script, err := daemon.ResolveDaemonScript(h.cfg.ResourcesDir); err != nil {
		log.Printf("[host] daemon script not found: %v (run `pnpm gulp` to build static/)", err)
	} else if h.cfg.ExpectedRevision == "" {
		h.cfg.ExpectedRevision = extractDaemonRevision(script)
	}

	daemonCfg := daemon.Config{
		GraphsDir:        h.cfg.GraphsDir,
		OwnerSource:      "electron",
		ExpectedRevision: h.cfg.ExpectedRevision,
		ScriptPath:       h.daemonScript(),
		NodeBin:          h.cfg.NodeBin,
	}
	h.initManager(daemonCfg)

	// Stop outdated workers from older app revisions before first window.
	if storage, err := h.ensureStorage(); err != nil {
		log.Printf("[host] resolve storage: %v", err)
	} else if h.cfg.ExpectedRevision != "" {
		if err := lifecycle.StopOutdatedWorkers(storage, h.cfg.ExpectedRevision, ""); err != nil {
			log.Printf("[host] stop outdated workers: %v", err)
		}
	}

	if _, err := h.createWindow(createWindowOpts{}); err != nil {
		log.Fatalf("[host] create main window: %v", err)
	}
}

func (h *Host) daemonScript() string {
	if script, err := daemon.ResolveDaemonScript(h.cfg.ResourcesDir); err == nil {
		return script
	}
	return ""
}

// onSecondInstance: Electron switches to the existing window and processes
// a logseq: deeplink found in argv (core.cljs single-instance).
func (h *Host) onSecondInstance(args []string) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "logseq:") {
			h.handleDeeplink(arg)
			return
		}
	}
	h.focusFirstWindow()
}

func (h *Host) focusFirstWindow() {
	if id, ok := h.findWindowOnGraph(""); ok {
		h.focusWindowByID(id)
		return
	}
	for _, hd := range h.allHandles() {
		if hd.win != nil && !hd.win.IsDestroyed() {
			hd.win.Show()
			hd.win.Focus()
			return
		}
	}
}

// handleDeeplink implements the logseq-url-handler subset:
//   - logseq://graph/<identifier>            → open/redirect graph
//   - logseq://new-window                    → focused window
//   - logseq://x-callback-url/quickCapture   → quickCapture event
//   - logseq://x-callback-url/invokeCommand  → invokeCommand event
//   - logseq://handbook/<key>                → handbook event
func (h *Host) handleDeeplink(rawURL string) {
	log.Printf("[host] deeplink %s", rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil {
		h.notifyError(nil, "Cannot parse url: "+rawURL, "", nil)
		return
	}
	host := strings.ToLower(parsed.Host)
	query := parsed.Query()
	switch host {
	case "graph":
		h.openGraphFromLink(parsed, false)
	case "new-window":
		// url.cljs: new-window is local-url-handler with force-new-window?,
		// so the path carries the graph identifier; a bare URL errors.
		h.openGraphFromLink(parsed, true)
	case "x-callback-url":
		switch strings.TrimPrefix(parsed.Path, "/") {
		case "quickCapture":
			h.emitToAll(evQuickCapture, map[string]any{
				"url":     query.Get("url"),
				"title":   query.Get("title"),
				"content": query.Get("content"),
				"page":    query.Get("page"),
				"append":  query.Get("append") == "true",
			})
		case "invokeCommand":
			h.emitToAll(evInvokeCommand, map[string]any{
				"action":  query.Get("action"),
				"payload": query.Get("payload"),
			})
		default:
			h.notifyError(nil, fmt.Sprintf("Unimplemented x-callback-url action %s", parsed.Path), "", nil)
		}
	case "handbook":
		key := strings.TrimPrefix(parsed.Path, "/")
		args := map[string]any{}
		for k, v := range query {
			if len(v) > 0 {
				args[k] = v[0]
			}
		}
		h.emitToAll(evHandbook, map[string]any{"key": key, "args": args})
	default:
		h.notifyError(nil, fmt.Sprintf("Cannot match `%s`", host), "", nil)
	}
}

// openGraphFromLink resolves logseq://graph/<identifier> and mirrors
// url.cljs local-url-handler: a window already on the graph is focused and
// redirected to the requested page/block; otherwise a new window is opened
// (renderer-driven, or directly when none is alive) and the redirect rides
// the pendingRedirect slot until that window reports graphReady.
func (h *Host) openGraphFromLink(parsed *url.URL, forceNewWindow bool) {
	// url.cljs replaces every "/" in the pathname, then decodes.
	identifier := strings.ReplaceAll(parsed.Path, "/", "")
	if decoded, err := url.PathUnescape(identifier); err == nil {
		identifier = decoded
	}
	graphName := h.resolveGraphName(identifier)
	if graphName == "" {
		h.graphIdentifierError(identifier)
		return
	}
	query := parsed.Query()
	pageName, blockID, file := query.Get("page"), query.Get("block-id"), query.Get("file")
	hasTarget := pageName != "" || blockID != "" || file != ""
	nilIfEmpty := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}

	graphDir := h.resolveGraphDir(graphName)
	id, onGraph := h.findWindowOnGraph(graphDir)
	openNewWindow := forceNewWindow || !onGraph
	if hasTarget {
		// Missing keys must be null (not ""): empty strings are truthy in
		// CLJS and the renderer's cond would take the page-name branch.
		payload := map[string]any{"page-name": nilIfEmpty(pageName), "block-id": nilIfEmpty(blockID), "file": nilIfEmpty(file)}
		if openNewWindow {
			h.setPendingRedirect(graphName, payload)
		} else if hd := h.windowByID(id); hd != nil {
			h.focusWindowByID(id)
			h.emitTo(hd.win, evRedirectExists, payload)
		}
	}
	if !openNewWindow {
		return
	}
	// Electron asks a renderer to open the window (openNewWindowOfGraph →
	// openNewWindow ipc). With no window alive there is no renderer to ask,
	// so create it directly.
	if h.countWindows() == 0 {
		if _, err := h.createWindow(createWindowOpts{Graph: graphName}); err != nil {
			log.Printf("[host] deeplink window: %v", err)
		}
		return
	}
	if win := h.focusedOrFirstWindow(); win != nil {
		// listener.cljs openNewWindowOfGraph takes the bare repo string.
		h.emitTo(win, evNewWinOfGraph, graphName)
	}
}

// graphIdentifierError mirrors url.cljs graph-identifier-error-handler.
func (h *Host) graphIdentifierError(identifier string) {
	if identifier != "" {
		h.notifyError(nil,
			fmt.Sprintf("Failed to open link. Cannot match graph identifier `%s` to any linked graph.", identifier),
			":electron/link-open-failed-no-graph", []any{identifier})
		return
	}
	h.notifyError(nil, "Failed to open link. Missing graph identifier after `logseq://graph/`.",
		":electron/link-open-failed-missing-graph", nil)
}

// focusedOrFirstWindow returns the focused window, falling back to the first
// alive window (Electron send-to-renderer drops the message instead; we keep
// the deeplink actionable).
func (h *Host) focusedOrFirstWindow() *mygoWindow {
	for _, hd := range h.allHandles() {
		if hd.win != nil && !hd.win.IsDestroyed() && hd.win.IsFocused() {
			return hd.win
		}
	}
	for _, hd := range h.allHandles() {
		if hd.win != nil && !hd.win.IsDestroyed() {
			return hd.win
		}
	}
	return nil
}

// focusWindowByID shows and focuses the window, if it still exists.
func (h *Host) focusWindowByID(id int) {
	if hd := h.windowByID(id); hd != nil && hd.win != nil && !hd.win.IsDestroyed() {
		hd.win.Show()
		hd.win.Focus()
	}
}

var _ = runtime.GOOS
var _ = http.StatusOK
