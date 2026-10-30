package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"logseq/mygo-host/internal/edn"
	"logseq/mygo-host/internal/lifecycle"
	"logseq/mygo-host/internal/lqgraph"
	"logseq/mygo-host/internal/transit"
)

// LqMain handles the "main" IPC channel: the renderer sends the payload
// [cmd, args...] (or a legacy transit string), we answer with plain
// JSON-able values (the renderer keywordizes map keys itself).
type LqMain struct{ H *Host }

// Handle dispatches one command. payload is either []any or (legacy) a
// transit-encoded string.
func (l *LqMain) Handle(ctx context.Context, payload any) (any, error) {
	if b, err := json.Marshal(payload); err == nil {
		s := string(b)
		if len(s) > 200 {
			s = s[:200] + "..."
		}
		log.Printf("[ipc] payload %T: %s", payload, s)
	} else {
		log.Printf("[ipc] payload %T (unmarshalable)", payload)
	}
	var msg []any
	switch p := payload.(type) {
	case []any:
		msg = p
	case string:
		decoded, err := transit.DecodeArray(p)
		if err != nil {
			return nil, fmt.Errorf("bad ipc payload: %w", err)
		}
		msg = decoded
	default:
		return nil, fmt.Errorf("bad ipc payload type %T", payload)
	}
	// Strip the ":js-obj" trailing marker (renderer hints "raw object");
	// Go always returns plain JSON so it is a no-op.
	if n := len(msg); n > 0 {
		if s, ok := msg[n-1].(string); ok && (s == ":js-obj" || s == "js-obj") {
			msg = msg[:n-1]
		}
	}
	if len(msg) == 0 {
		return nil, errors.New("empty ipc message")
	}
	cmd := strings.TrimPrefix(stringArg(msg, 0), ":")
	log.Printf("[ipc] -> %s (%d args)", cmd, len(msg)-1)
	res, err := l.dispatch(ctx, cmd, msg[1:])
	if err != nil {
		log.Printf("[ipc] <- %s ERROR: %v", cmd, err)
	} else {
		log.Printf("[ipc] <- %s ok", cmd)
	}
	return res, err
}

func (l *LqMain) dispatch(ctx context.Context, cmd string, args []any) (any, error) {
	h := l.H
	w := mygo.CallerWindow(ctx)
	switch cmd {
	// ---- fs ----
	case "mkdir":
		return nilOnly(os.Mkdir(stringArg(args, 0), 0o755))
	case "mkdir-recur":
		return nilOnly(os.MkdirAll(stringArg(args, 0), 0o755))
	case "readdir":
		return lqgraphReaddir(stringArg(args, 0)), nil
	case "listdir":
		return deepReadDir(stringArg(args, 0), boolArg(args, 1, true))
	case "unlink":
		return h.unlinkToRecycle(stringArg(args, 0), stringArg(args, 1)), nil
	case "openFileInFolder":
		mygoShellShowItemInFolder(stringArg(args, 0))
		return nil, nil
	case "readFile":
		return readStringFile(stringArg(args, 0)), nil
	case "readFileRaw":
		return readRawFile(stringArg(args, 0))
	case "copyFile":
		return nil, copyPath(stringArg(args, 0), stringArg(args, 1))
	case "writeFile":
		return h.writeFileCmd(stringArg(args, 0), stringArg(args, 1), argOf(args, 2))
	case "writeFileBytes":
		return h.writeFileBytesCmd(stringArg(args, 0), stringOf(maybeMap(argOf(args, 1)), "__lqBytes"))
	case "rename":
		return nilOnly(os.Rename(stringArg(args, 0), stringArg(args, 1)))
	case "stat":
		return statOf(stringArg(args, 0))
	case "openDialog":
		return h.openDirDialog(w)
	case "showOpenDialog":
		return h.showOpenDialog(w, mapArg(argOf(args, 0)))

	// ---- graphs ----
	case "getGraphs":
		return l.getGraphs()
	case "get-graph-name":
		return l.getGraphName(stringArg(args, 0)), nil
	case "upsertGraphRegistryEntry":
		entry, _ := argOf(args, 0).(map[string]any)
		return nil, h.upsertGraphRegistryEntry(entry)
	case "deleteGraph":
		return h.deleteGraphCmd(stringArg(args, 0))
	case "createGraph":
		return h.createGraphCmd(stringArg(args, 0))

	// ---- db worker runtime ----
	case "db-worker-runtime":
		opts, _ := argOf(args, 1).(map[string]any)
		return h.dbWorkerRuntimeCmd(w, stringArg(args, 0), opts)
	case "releaseDbWorkerRuntime":
		return nil, h.mgrReleaseRuntime(stringArg(args, 0))
	case "db-export":
		return h.dbExportCmd(stringArg(args, 0), boolArg(args, 1, false))
	case "db-export-as":
		return h.dbExportAsCmd(stringArg(args, 0), stringArg(args, 1))
	case "db-get":
		return h.dbGetCmd(stringArg(args, 0))
	case "setCurrentGraph":
		return h.setCurrentGraphCmd(w, stringArg(args, 0))
	case "getAssetsFiles":
		return h.getAssetsFilesCmd(w, mapArg(argOf(args, 0))), nil

	// ---- dialogs / dirs ----
	case "openDir":
		return h.openDirCmd(w) // returns (any, error)
	case "getFiles":
		return h.getFilesCmd(stringArg(args, 0))

	// ---- network ----
	case "setProxy":
		opts, _ := argOf(args, 0).(map[string]any)
		_, err := h.setProxyCmd(opts)
		return nil, err
	case "testProxyUrl":
		opts, _ := argOf(args, 1).(map[string]any)
		return h.testProxyURLCmd(stringArg(args, 0), opts)
	case "httpFetchJSON":
		opts, _ := argOf(args, 1).(map[string]any)
		return h.httpFetchJSONCmd(stringArg(args, 0), opts)
	case "httpRequest":
		opts, _ := argOf(args, 1).(map[string]any)
		return h.httpRequestCmd(stringArg(args, 0), opts)
	case "httpRequestAbort":
		h.httpRequestAbortCmd(stringArg(args, 0))
		return nil, nil

	// ---- app / os ----
	case "getLogseqDotDirRoot":
		return h.DotRoot(), nil
	case "getUserDefaultPlugins":
		return h.userDefaultPlugins(), nil
	case "installMarketPlugin":
		item, _ := argOf(args, 0).(map[string]any)
		return nilOnly(h.installMarketPluginCmd(w, item))
	case "updateMarketPlugin":
		pkg, _ := argOf(args, 0).(map[string]any)
		return nilOnly(h.installMarketPluginCmd(w, pkg))
	case "uninstallMarketPlugin":
		return nilOnly(h.uninstallMarketPluginCmd(stringArg(args, 0)))
	case "validateUserExternalPlugins":
		urls, _ := argOf(args, 0).([]any)
		return h.validateUserExternalPlugins(urls)
	case "relaunchApp":
		mygoAppRelaunch()
		return nil, nil
	case "quitApp":
		mygoAppQuit()
		return nil, nil
	case "userAppCfgs":
		return h.userAppCfgsCmd(args)
	case "getAppBaseInfo":
		return h.appBaseInfo(w), nil
	case "updateElectronLocale":
		h.setLocale(stringArg(args, 0))
		return nil, nil
	case "runCli":
		opts, _ := argOf(args, 0).(map[string]any)
		return h.runCliCmd(opts)
	case "system/info":
		return map[string]any{"home-dir": homeDir(), "graphs-dir": h.cfg.GraphsDir}, nil

	// ---- windows ----
	case "openNewWindow":
		return nil, h.openNewWindowCmd(stringArg(args, 0))
	case "graphHasOtherWindow":
		return h.graphHasOtherWindowCmd(w, stringArg(args, 0)), nil
	case "graphHasMultipleWindows":
		return h.graphHasMultipleWindowsCmd(stringArg(args, 0)), nil
	case "graphReady":
		return nil, h.graphReadyCmd(w, stringArg(args, 0))
	case "window-minimize", "window-toggle-maximized", "window-toggle-fullscreen", "window-close",
		"set-window-title", "theme-loaded", "find-in-page", "clear-find-in-page":
		return h.windowCommand(w, cmd, args)

	// ---- keychain ----
	case "keychain/save-e2ee-password":
		return nil, h.keychainSet(stringArg(args, 0), stringArg(args, 1))
	case "keychain/get-e2ee-password":
		return h.keychainGet(stringArg(args, 0))
	case "keychain/delete-e2ee-password":
		return nil, h.keychainDelete(stringArg(args, 0))

	// ---- clipboard (preload helpers) ----
	case "getFilePathFromClipboard":
		return clipboardFilePath()
	case "getClipboardData":
		return clipboardData(stringArg(args, 0))
	case "setZoomFactor":
		if w != nil {
			if n, ok := numberOf(maybeNumberArg(argOf(args, 0)), "v"); ok {
				w.Page().SetZoomFactor(n)
			}
		}
		return nil, nil
	case "setZoomLevel":
		return nil, nil

	// ---- server (db-sync local) — milestone 1 stubs ----
	case "server/load-state", "server/do", "server/set-config":
		return nil, nil

	default:
		return nil, fmt.Errorf("Error: no ipc handler for: %s", cmd)
	}
}

func nilOnly(err error) (any, error) { return nil, err }

// ---- fs helpers ----

func readStringFile(path string) any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return string(data)
}

func readRawFile(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil // Electron read-file-raw returns empty on error
	}
	return map[string]any{"__lqBytes": base64.StdEncoding.EncodeToString(data)}, nil
}

func copyPath(from, to string) error {
	st, err := os.Stat(from)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return copyDir(from, to)
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o644)
}

func copyDir(from, to string) error {
	return filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

func statOf(path string) (any, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"size":      st.Size(),
		"birthtime": st.ModTime().Format(time.RFC3339Nano),
		"mtime":     st.ModTime().Format(time.RFC3339Nano),
		"ctime":     st.ModTime().Format(time.RFC3339Nano),
	}, nil
}

// lqgraphReaddir ports common-graph/readdir: recursive relative paths,
// skipping symlinks and dot-prefixed names.
func lqgraphReaddir(root string) []string {
	var out []string
	root = strings.TrimSuffix(root, string(os.PathSeparator))
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == root {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}

// deepReadDir ports js-utils/deepReadDir (flat: string list of file paths).
func deepReadDir(dir string, flat bool) (any, error) {
	if dir == "" {
		return nil, nil
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, nil
	}
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if flat {
		return out, nil
	}
	return [][]string{out}, nil
}

func (h *Host) unlinkToRecycle(repoDir, path string) any {
	if repoDir == "" || path == "" {
		return nil
	}
	// Plugin dot-dir and asset files are deleted outright.
	if strings.HasPrefix(filepath.ToSlash(filepath.Clean(path)), filepath.ToSlash(h.DotRoot())+"/") ||
		strings.Contains(filepath.ToSlash(path), "assets/storages") {
		_ = os.Remove(path)
		return nil
	}
	name := strings.TrimPrefix(path, repoDir+string(os.PathSeparator))
	name = strings.TrimPrefix(path, repoDir+"/")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	recycle := filepath.Join(repoDir, "logseq", ".recycle")
	_ = os.MkdirAll(recycle, 0o755)
	_ = os.Rename(path, filepath.Join(recycle, name))
	return nil
}

func (h *Host) writeFileCmd(repo, path string, content any) (any, error) {
	var data []byte
	switch c := content.(type) {
	case string:
		data = []byte(c)
	case map[string]any:
		b64 := stringOf(c, "__lqBytes")
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, err
		}
		data = decoded
	default:
		return nil, fmt.Errorf("writeFile: unsupported content type %T", content)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode()&0o200 == 0 {
		// enable-automatic-chmod? default true: make writable then write.
		_ = os.Chmod(path, 0o644)
	}
	err := os.WriteFile(path, data, 0o644)
	if err != nil {
		// Best-effort backup of the previous content, then notify.
		backup := h.backupFailedWrite(repo, path)
		msg := fmt.Sprintf("Write to the file %s failed, %v.", path, err)
		if backup != "" {
			msg = fmt.Sprintf("Write to the file %s failed, %v. A backup file was saved to %s.", path, err, backup)
			h.notifyErrorArgs(nil, msg, "electron/write-file-error-with-backup", []any{path, err.Error(), backup})
		} else {
			h.notifyErrorArgs(nil, msg, "electron/write-file-error", []any{path, err.Error()})
		}
		return nil, nil // Electron swallows the error after backing up
	}
	return statOf(path)
}

func (h *Host) writeFileBytesCmd(path, b64 string) (any, error) {
	if b64 == "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(path, nil, 0o644)
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return nil, os.WriteFile(path, data, 0o644)
}

// backupFailedWrite mirrors electron.backup-file/backup-file: copy the old
// file into <repo>/backups/… as a timestamped sibling.
func (h *Host) backupFailedWrite(repo, path string) string {
	if repo == "" {
		return ""
	}
	old, err := os.ReadFile(path)
	if err != nil {
		old = nil
	}
	dir := filepath.Join(repo, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	name := filepath.Base(path) + "." + time.Now().UTC().Format("2006-01-02T15_04_05.000Z") + ".bak"
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, old, 0o644); err != nil {
		return ""
	}
	return dst
}

// ---- graph helpers ----

func (l *LqMain) getGraphs() (any, error) {
	graphs, err := dbBasedGraphs(l.H.cfg.GraphsDir)
	if err != nil {
		return nil, err
	}
	return graphs, nil
}

func (l *LqMain) getGraphName(identifier string) string {
	return l.H.resolveGraphName(identifier)
}

func (h *Host) resolveGraphName(identifier string) string {
	if identifier == "" {
		return ""
	}
	// Registry exact/normalized match first.
	if repo := lqgraph.ResolveRegistryTarget(h.readGraphRegistry(), identifier); repo != "" {
		return repo
	}
	// Fallback: canonical repo matched against known graphs.
	repo := lqgraph.CanonicalRepo(identifier)
	graphs, _ := dbBasedGraphs(h.cfg.GraphsDir)
	lc := lqgraph.NormalizeLC(repo)
	stripped := lqgraph.StripDBVersionPrefix(lc)
	for _, g := range graphs {
		glc := lqgraph.NormalizeLC(g)
		if glc == lc || strings.HasSuffix(glc, "/"+stripped) {
			return g
		}
	}
	return ""
}

func (h *Host) readGraphRegistry() []map[string]any {
	data, err := os.ReadFile(filepath.Join(h.DotRoot(), "graphs.edn"))
	if err != nil {
		return nil
	}
	parsed, err := edn.Parse(string(data))
	if err != nil {
		return nil
	}
	out := []map[string]any{}
	for _, v := range edn.VectorOf(parsed) {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (h *Host) upsertGraphRegistryEntry(entry map[string]any) error {
	graphID := ednKeywordString(entry, "graph-id")
	if graphID == "" {
		graphID = ednKeywordString(entry, "local-graph-id")
	}
	if graphID == "" {
		return errors.New("Missing graph identity")
	}
	delete(entry, "rtc-graph-id")
	if s, ok := entry["repo"].(string); ok {
		entry["repo"] = strings.TrimSpace(s)
	}
	if s, ok := entry["graph-name"].(string); ok {
		entry["graph-name"] = strings.TrimSpace(s)
	}
	registry := h.readGraphRegistry()
	registry = lqgraph.UpsertRegistryEntry(registry, entry)
	if err := os.MkdirAll(h.DotRoot(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.DotRoot(), "graphs.edn"), []byte(edn.Print(registry)), 0o644)
}

func ednKeywordString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(strings.TrimPrefix(v, ":"))
}

// dbBasedGraphs ports common-graph/get-db-based-graphs.
func dbBasedGraphs(graphsDir string) ([]string, error) {
	if err := os.MkdirAll(graphsDir, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(graphsDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if name == "Unlinked graphs" {
			continue
		}
		decoded := lqgraph.DecodeGraphName(name)
		if decoded == "" || strings.HasPrefix(decoded, "logseq_local_") {
			continue
		}
		out = append(out, lqgraph.CanonicalRepo(decoded))
	}
	return lqgraph.Distinct(out), nil
}

// ---- graph lifecycle commands ----

func (h *Host) deleteGraphCmd(graph string) (any, error) {
	repo := lqgraph.CanonicalRepo(graph)
	storage, err := h.ensureStorage()
	if err != nil {
		return nil, err
	}
	snapshot, err := lifecycle.Snapshot(storage, repo)
	generation := ""
	if err == nil && snapshot != nil {
		generation = stringOf(snapshot, "generation")
	}
	h.mgrInvalidateRepo(repo)
	h.notifyGraphLifecycle(repo, "deleting", generation)
	existed, destination, err := lifecycle.DeleteGraph(storage, repo)
	if err != nil {
		return nil, err
	}
	if !existed {
		return nil, errors.New("Graph does not exist")
	}
	h.notifyGraphLifecycle(repo, "deleted", generation)
	return map[string]any{"ok": true, "destination": destination}, nil
}

func (h *Host) createGraphCmd(repo string) (any, error) {
	storage, err := h.ensureStorage()
	if err != nil {
		return nil, err
	}
	gen, err := lifecycle.CreateGraph(storage, lqgraph.CanonicalRepo(repo))
	if err != nil {
		return nil, err
	}
	return gen, nil
}

// ---- misc ----

func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

func mygoShellShowItemInFolder(path string) { mygoShell().ShowItemInFolder(path) }

var _ = exec.Command // keep os/exec import if unused later
