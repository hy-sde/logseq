package host

import (
	"regexp"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo"

)

// ---- platform / mygo accessors ----

func platformName() string { return runtime.GOOS }

func runtimeArch() string { return runtime.GOARCH }

func timeNow() time.Time { return time.Now() }

func mygoSecrets() mygo.SecretsModule { return mygo.Secrets.Service("Logseq E2EE") }

func errSecretNotFound() error { return mygo.ErrSecretNotFound }

func mygoShell() mygo.ShellModule { return mygo.Shell }

func mygoAppRelaunch() { mygo.App.Relaunch() }

func mygoAppQuit() { mygo.App.Quit() }

// ---- dialogs ----

// openDirDialog ports handler open-dir-dialog: directory picker,
// first path or "".
func (h *Host) openDirDialog(w *mygoWindow) (string, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:    w,
		Title:     "Open directory",
		Directory: true,
	})
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", nil
	}
	return paths[0], nil
}

// showOpenDialog passes options through (renderer-shape: keys like
// properties are ignored; Directory/Multiple honored when present).
func (h *Host) showOpenDialog(w *mygoWindow, opts map[string]any) (any, error) {
	dialogOpts := mygo.OpenDialogOptions{Parent: w}
	if props, ok := opts["properties"].([]any); ok {
		for _, p := range props {
			switch s, _ := p.(string); s {
			case "openDirectory":
				dialogOpts.Directory = true
			case "multiSelections", "openFile", "openFiles":
				dialogOpts.Multiple = true
			case "showHiddenFiles":
				dialogOpts.ShowHiddenFiles = true
			}
		}
	}
	if v, ok := opts["title"].(string); ok {
		dialogOpts.Title = v
	}
	if v, ok := opts["defaultPath"].(string); ok {
		dialogOpts.DefaultPath = v
	}
	paths, err := mygo.Dialog.Open(dialogOpts)
	if err != nil {
		return nil, err
	}
	if paths == nil {
		paths = []string{}
	}
	return map[string]any{"canceled": len(paths) == 0, "filePaths": paths}, nil
}

// ---- find in page options ----

func findOptionsOf(v any) mygo.FindOptions {
	opts := mygo.FindOptions{}
	if m, ok := v.(map[string]any); ok {
		if b, ok := m["findNext"].(bool); ok {
			opts.FindNext = b
		}
	}
	return opts
}

// ---- runCli exec helpers ----

func runCliContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 120*time.Second)
}

func execCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

type execExitError = exec.ExitError

// ---- graph file listing (common-graph port, host side) ----

// lqgraphGraphFiles returns relative slash paths of recognized files under
// graphDir, mirroring logseq.common.graph/get-files ignoring rules.
func lqgraphGraphFiles(graphDir string) ([]string, error) {
	allowed := map[string]bool{".org": true, ".markdown": true, ".md": true, ".edn": true, ".json": true, ".js": true, ".css": true}
	var out []string
	root := filepath.Clean(graphDir)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // missing paths ignored
		}
		name := d.Name()
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		rpath := filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		// Ignoring rules from logseq.common.graph/ignored-path?
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if name == ".DS_Store" || strings.HasSuffix(name, ".DS_Store") {
			return nil
		}
		switch rpath {
		case "logseq/graphs-txid.edn", "logseq/pages-metadata.edn":
			return nil
		}
		if strings.Contains(rpath, "logseq/.recycle/") ||
			strings.Contains(rpath, "logseq/bak/") ||
			strings.Contains(rpath, "logseq/version-files/") ||
			strings.Contains(rpath, "mirror/markdown") ||
			strings.Contains(rpath, "/node_modules/") {
			return nil
		}
		if hiddenReMatch(rpath) {
			return nil
		}
		if !allowed[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		out = append(out, rpath)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (h *Host) relaunchAppCmd() { mygoAppRelaunch() }

func (h *Host) quitAppCmd() { mygoAppQuit() }

func (h *Host) systemInfo() map[string]any {
	return map[string]any{
		"home-dir":   homeDir(),
		"graphs-dir": h.cfg.GraphsDir,
	}
}
func (h *Host) getLogseqDotDirRoot() string { return h.DotRoot() }

// setProxyCmd stores proxy options (milestone 1: used by HTTP clients only).
func (h *Host) setProxyCmd(opts map[string]any) (any, error) {
	h.mu.Lock()
	h.proxyOptions = opts
	h.mu.Unlock()
	return nil, nil
}

// openFileInFolderCmd reveals a path in the file manager.
func (h *Host) openFileInFolderCmd(path string) {
	mygoShell().ShowItemInFolder(path)
}

// openExternalCmd with protocol allowlist.
func (h *Host) openExternalCmd(url string) error {
	lower := strings.ToLower(url)
	allowed := false
	for _, p := range []string{"http://", "https://", "mailto:", "zotero://", "file://"} {
		if strings.HasPrefix(lower, p) {
			allowed = true
			break
		}
	}
	if !allowed {
		return errors.New("illegal protocol")
	}
	return mygoShell().OpenExternal(url)
}

// openPathCmd expands ~ like Electron openPath.
func (h *Host) openPathCmd(p string) error {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		home := homeDir()
		if home != "" {
			p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), string(filepath.Separator)))
		}
	}
	return mygoShell().OpenPath(p)
}

// quitAndInstall / updater stubs (milestone 1: no auto-update).
func (h *Host) updaterStub() map[string]any {
	return map[string]any{"type": "update-not-available"}
}

var hiddenSegmentRe = regexp.MustCompile(`(^|/)\.[^.]+`)

func hiddenReMatch(rpath string) bool { return hiddenSegmentRe.MatchString(rpath) }

