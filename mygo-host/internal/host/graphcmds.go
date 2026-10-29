package host

import (
	"sync"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"logseq/mygo-host/internal/lifecycle"
	"logseq/mygo-host/internal/lqgraph"
	"logseq/mygo-host/internal/transit"
)

// ---- db worker runtime commands ----

func (h *Host) dbWorkerRuntimeCmd(w *mygoWindow, repo string, opts map[string]any) (any, error) {
	if repo == "" {
		return nil, errors.New("repo is required")
	}
	generation := stringOf(opts, "generation")
	rt, err := h.mgr.EnsureStarted(repo, 0, generation)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"repo":       rt.Repo,
		"root-dir":   rt.RootDir,
		"generation": rt.Generation,
		"base-url":   rt.BaseURL,
		"auth-token": rt.AuthToken, // nil in milestone 1 (renderer handles absent)
		"owned?":     rt.Owned,
	}, nil
}

func (h *Host) mgrReleaseRuntime(repo string) error {
	if repo == "" {
		return errors.New("repo is required")
	}
	return h.mgr.ReleaseRuntime(repo)
}

func (h *Host) mgrInvalidateRepo(repo string) {
	if repo != "" {
		h.mgr.InvalidateRepo(repo)
	}
}

// ---- db export / get (worker HTTP) ----

// invokeWorker calls the daemon's /v1/invoke endpoint and decodes the
// transit result.
func (h *Host) invokeWorker(baseURL, method string, args []any) (any, error) {
	body := map[string]any{"method": method, "args": args}
	resp, err := httpClient().Post(baseURL+"/v1/invoke", "application/json", jsonReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := readAllBody(resp.Body)
	if err != nil {
		return nil, err
	}
	parsed, err := lifecycle.ParseJSONBody(data)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		errMap, _ := parsed["error"].(map[string]any)
		return nil, fmt.Errorf("%s", stringOf(errMap, "message"))
	}
	result := stringOf(parsed, "resultTransit")
	if result == "" {
		return nil, nil
	}
	return transit.Decode(result)
}

func (h *Host) dbExportCmd(repo string, forceBackup bool) (any, error) {
	repo = lqgraph.CanonicalRepo(repo)
	if repo == "" {
		return nil, errors.New("repo is required")
	}
	graphsDir := h.cfg.GraphsDir
	enc, _ := lqgraph.EncodeGraphName(lqgraph.RepoIdentity(repo))
	if err := os.MkdirAll(filepath.Join(graphsDir, enc), 0o755); err != nil {
		return nil, err
	}
	// Ensure a runtime for the backup snapshot.
	rt, err := h.mgr.EnsureStarted(repo, 0, "")
	if err != nil {
		return nil, err
	}
	// graph-backup reserve-next-backup-target! port:
	// <graphs-dir>/<enc>/backup/<encoded-name>[-suffix]/db.sqlite
	backupRoot := filepath.Join(graphsDir, enc, "backup")
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		return nil, err
	}
	ts := timeNowUTC("20060102T150405Z")
	base, _ := lqgraph.EncodeGraphName(lqgraph.RepoIdentity(repo) + "-" + ts)
	dir := ""
	for suffix := 0; ; suffix++ {
		name := base
		if suffix > 0 {
			name = fmt.Sprintf("%s-%d", base, suffix)
		}
		candidate := filepath.Join(backupRoot, name)
		err := os.Mkdir(candidate, 0o755)
		if err == nil {
			dir = candidate
			break
		}
		if !os.IsExist(err) {
			return nil, err
		}
	}
	dbPath := filepath.Join(dir, "db.sqlite")
	result, err := h.invokeWorker(rt.BaseURL, "thread-api/backup-db-sqlite", []any{repo, dbPath})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"path": dbPath, "backup-name": filepath.Base(dir), "created?": true}
	if m, ok := result.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	return out, nil
}

func (h *Host) dbExportAsCmd(repo, filename string) (any, error) {
	repo = lqgraph.CanonicalRepo(repo)
	if repo == "" {
		return nil, errors.New("repo is required")
	}
	rt, err := h.mgr.EnsureStarted(repo, 0, "")
	if err != nil {
		return nil, err
	}
	enc, _ := lqgraph.EncodeGraphName(lqgraph.RepoIdentity(repo))
	exportDir := filepath.Join(h.cfg.GraphsDir, enc, "export")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(exportDir, filepath.Base(filename))
	if _, err := h.invokeWorker(rt.BaseURL, "thread-api/backup-db-sqlite", []any{repo, dst}); err != nil {
		return nil, err
	}
	return map[string]any{"path": dst}, nil
}

func (h *Host) dbGetCmd(repo string) (any, error) {
	repo = lqgraph.CanonicalRepo(repo)
	if repo == "" {
		return nil, errors.New("repo is required")
	}
	// Legacy path: raw db.sqlite bytes (renderer warns this is compat-only).
	enc, _ := lqgraph.EncodeGraphName(lqgraph.RepoIdentity(repo))
	dbPath := filepath.Join(h.cfg.GraphsDir, enc, "db.sqlite")
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return nil, nil
	}
	return map[string]any{"__lqBytes": base64.StdEncoding.EncodeToString(data)}, nil
}

func (h *Host) setCurrentGraphCmd(w *mygoWindow, graphName string) (any, error) {
	if w == nil {
		return nil, nil
	}
	var nextPath string
	if graphName != "" {
		nextPath = h.resolveGraphDir(graphName)
		if nextPath == "" {
			return nil, fmt.Errorf("graph dir not found for %s", graphName)
		}
	}
	prev, _ := h.windowGraph(wID(w))
	if prev != "" && nextPath != "" && prev != nextPath {
		_ = h.mgr.EnsureWindowStopped(w.ID())
	}
	if graphName != "" {
		// Sync auto backup bookkeeping happens inside the manager.
		h.setWindowGraph(w.ID(), nextPath, graphName)
		// NOTE: no evSetCurGraph.Emit here — Electron's :setCurrentGraph
		// handler never pushes back to the renderer; emitting to the sender
		// makes the renderer's "setCurrentGraph" listener re-call this IPC
		// forever (346k calls in 70s during smoke21).
	} else {
		w.Close()
	}
	return nil, nil
}

func (h *Host) resolveGraphDir(graphName string) string {
	trimmed := strings.TrimSpace(graphName)
	if !strings.HasPrefix(trimmed, "logseq_db_") {
		return ""
	}
	repo := lqgraph.CanonicalRepo(trimmed)
	if repo == "" {
		return ""
	}
	enc, err := lqgraph.EncodeGraphName(lqgraph.RepoIdentity(repo))
	if err != nil {
		return ""
	}
	return filepath.Join(h.cfg.GraphsDir, enc)
}

func (h *Host) getAssetsFilesCmd(w *mygoWindow, opts map[string]any) any {
	exts := []string{}
	if raw, ok := opts["exts"].([]any); ok {
		for _, e := range raw {
			if s, ok := e.(string); ok && s != "" {
				if !strings.HasPrefix(s, ".") {
					s = "." + s
				}
				exts = append(exts, strings.ToLower(s))
			}
		}
	}
	graphDir, _ := h.windowGraph(wID(w))
	if graphDir == "" {
		return nil
	}
	assets := filepath.Join(graphDir, "assets")
	if st, err := os.Stat(assets); err != nil || !st.IsDir() {
		return nil
	}
	return getAllFiles(assets, exts)
}

// getAllFiles ports electron utils.js getAllFiles.
func getAllFiles(dir string, exts []string) []map[string]any {
	var out []map[string]any
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if len(exts) > 0 && !contains(exts, ext) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, map[string]any{
			"path":          p,
			"size":          info.Size(),
			"accessTime":    info.ModTime().UnixMilli(),
			"modifiedTime":  info.ModTime().UnixMilli(),
			"changeTime":    info.ModTime().UnixMilli(),
			"birthTime":     info.ModTime().UnixMilli(),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return stringOf(out[i], "path") < stringOf(out[j], "path") })
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---- open dir / files ----

func (h *Host) getFilesCmd(path string) (any, error) {
	files, err := h.collectGraphFiles(path)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "files": files}, nil
}

func (h *Host) openDirCmd(w *mygoWindow) (any, error) {
	path, err := h.openDirDialog(w)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("path empty")
	}
	files, err := h.collectGraphFiles(path)
	if err != nil {
		h.notifyErrorArgs(nil, "Opening the specified directory failed.\n"+err.Error(), "electron/open-dir-error", []any{err.Error()})
		return nil, err
	}
	return map[string]any{"path": path, "files": files}, nil
}

// collectGraphFiles ports common-graph/get-files + handler get-files body.
func (h *Host) collectGraphFiles(graphDir string) ([]map[string]any, error) {
	names, err := lqgraphGraphFiles(graphDir)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, p := range names {
		full := filepath.Join(graphDir, filepath.FromSlash(p))
		st, err := os.Stat(full)
		if err != nil || st.IsDir() {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"path":    p,
			"content": string(data),
			"stat":    map[string]any{"size": st.Size(), "mtime": st.ModTime().UnixMilli()},
		})
	}
	return out, nil
}

// ---- user plugin helpers ----

func (h *Host) userDefaultPlugins() []string {
	entries, err := os.ReadDir(h.PluginsRoot())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, filepath.Join(h.PluginsRoot(), name))
	}
	return out
}

func (h *Host) validateUserExternalPlugins(urls []any) (any, error) {
	out := map[string]any{}
	for _, u := range urls {
		s, _ := u.(string)
		if s == "" {
			continue
		}
		out[s] = fileExists(s) && fileExists(filepath.Join(s, "package.json"))
	}
	return out, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---- user configs (userData/configs.json; renderer-opaque in milestone 1) ----

func (h *Host) userAppCfgsCmd(args []any) (any, error) {
	var key any
	var value any
	hasKey := len(args) > 0 && argOf(args, 0) != nil
	if hasKey {
		key = argOf(args, 0)
		if len(args) > 1 && argOf(args, 1) != nil {
			value = argOf(args, 1)
			if err := h.setConfigItem(configKeyString(key), value); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return h.getConfigItem(configKeyString(key)), nil
	}
	return h.readConfig(), nil
}

func configKeyString(k any) string {
	if s, ok := k.(string); ok {
		return strings.TrimPrefix(s, ":")
	}
	return fmt.Sprintf("%v", k)
}

// ---- misc app info ----

func (h *Host) appBaseInfo(w *mygoWindow) map[string]any {
	full := w != nil && w.IsFullScreen()
	max := w != nil && w.IsMaximized()
	return map[string]any{
		"isFullScreen": full,
		"isMaximized":  max,
		"platform":     platformName(),
		"arch":         runtimeArch(),
	}
}

func (h *Host) setLocale(locale string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.locale = locale
}

func (h *Host) runCliCmd(opts map[string]any) (any, error) {
	command := stringOf(opts, "command")
	allowlist := []string{"git", "pandoc", "ag", "grep", "alda"}
	if !contains(allowlist, command) {
		return nil, fmt.Errorf("Shell: %s is not allowed!", command)
	}
	var cmdArgs []string
	if raw, ok := opts["args"].([]any); ok {
		for _, a := range raw {
			if s, ok := a.(string); ok {
				cmdArgs = append(cmdArgs, s)
			}
		}
	}
	ctx, cancel := runCliContext()
	defer cancel()
	cmd := execCommand(ctx, command, cmdArgs...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *execExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	if boolOf(opts, "returnResult") && len(out) > 0 {
		h.notifyErrorArgs(nil, fmt.Sprintf("Running %s: %s", command, strings.TrimSpace(string(out))), "", nil)
	}
	return code, nil
}

// ---- windows ----

func (h *Host) openNewWindowCmd(repo string) error {
	_, err := h.createWindow(createWindowOpts{Graph: repo})
	return err
}

func (h *Host) graphHasOtherWindowCmd(w *mygoWindow, graph string) bool {
	dir := h.resolveGraphDir(graph)
	if dir == "" {
		return false
	}
	except := wID(w)
	return h.countOtherWindows(dir, except) > 0
}

func (h *Host) graphHasMultipleWindowsCmd(graph string) bool {
	dir := h.resolveGraphDir(graph)
	if dir == "" {
		return false
	}
	return h.countWindowsOnGraph(dir) > 1
}

func (h *Host) windowCommand(w *mygoWindow, cmd string, args []any) (any, error) {
	if w == nil {
		return nil, nil
	}
	switch cmd {
	case "window-minimize":
		w.Minimize()
	case "window-toggle-maximized":
		if w.IsMaximized() {
			w.Unmaximize()
		} else {
			w.Maximize()
		}
	case "window-toggle-fullscreen":
		w.ToggleFullScreen()
	case "window-close":
		w.Close()
	case "set-window-title":
		if !w.IsDestroyed() {
			w.SetTitle(stringArg(args, 0))
		}
	case "theme-loaded":
		// windowStateKeeper managed by mygo StateKey persistence.
	case "find-in-page":
		res, err := w.Page().FindInPage(stringArg(args, 0), findOptionsOf(argOf(args, 1)))
		if err != nil {
			return nil, err
		}
		evFoundInPage.Emit(w, map[string]any{"matches": res.Matches, "active": res.Active})
		return map[string]any{"matches": res.Matches, "active": res.Active}, nil
	case "clear-find-in-page":
		w.Page().StopFindInPage()
	}
	return nil, nil
}

// ---- keychain ----

func (h *Host) keychainSet(key, value string) error {
	return mygoSecrets().Set(key, []byte(value))
}

func (h *Host) keychainGet(key string) (any, error) {
	data, err := mygoSecrets().Get(key)
	if err != nil {
		if errors.Is(err, errSecretNotFound()) {
			return nil, nil
		}
		return nil, nil // Electron keychain get returns nil on failure
	}
	return string(data), nil
}

func (h *Host) keychainDelete(key string) error {
	return mygoSecrets().Delete(key)
}

// ---- clipboard (called via LqMain from preload shims) ----

var clipboardMu sync.Mutex

// ---- shared small helpers ----

func timeNowUTC(layout string) string { return timeNow().UTC().Format(layout) }
