package host

// Marketplace plugin lifecycle — a port of Electron's
// src/electron/electron/plugin.cljs install-or-update!/uninstall! plus the
// GitHub release plumbing (valid-github-api!, fetch-release-asset,
// download-asset-zip). Only the Go standard library is used.

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	githubAPIPrimary = "https://api.github.com"
	githubAPIProxy   = "https://plugins.logseq.io/github/api"
	githubAPITTLC    = time.Minute
)

var (
	ghAPIMu      sync.Mutex
	ghAPIBase    string
	ghAPIProbed  time.Time
	reSlashes    = regexp.MustCompile(`^/+(.+?)/+$`)
	reLeadingDot = regexp.MustCompile(`^[./]+`)
)

// proxyClient builds the HTTP client used for marketplace + updater traffic.
// Explicit renderer proxy options are honored; otherwise Go's default
// (environment) proxy rules apply. socks5 is not supported by the stdlib
// transport (documented deviation, see httpclient.go).
func (h *Host) proxyClient(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	// setProxyCmd writes h.proxyOptions under h.mu; snapshot before use.
	h.mu.Lock()
	opts := h.proxyOptions
	h.mu.Unlock()
	if proxyURL := proxyURLFromOptions(opts); proxyURL != "" {
		transport := &http.Transport{}
		if pu, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(pu)
			client.Transport = transport
		}
	}
	return client
}

// probeGithubAPIBase ports plugin.cljs valid-github-api!: prefer the
// plugins.logseq.io GitHub API proxy when its rate_limit endpoint answers
// 200 with remaining quota, falling back to api.github.com. Re-probed at
// most once per minute.
func (h *Host) probeGithubAPIBase() string {
	ghAPIMu.Lock()
	defer ghAPIMu.Unlock()
	now := time.Now()
	if ghAPIBase != "" && now.Sub(ghAPIProbed) < githubAPITTLC {
		return ghAPIBase
	}
	if githubAPIUsable(h.proxyClient(2*time.Second), githubAPIProxy) {
		ghAPIBase = githubAPIProxy
	} else {
		ghAPIBase = githubAPIPrimary
	}
	ghAPIProbed = now
	log.Printf("[marketplace] using github api %s", ghAPIBase)
	return ghAPIBase
}

// githubAPIUsable ports electron_utils.github_api/proxy-usable?: the proxy
// answers 200 even when its own quota is spent, so resources.core.remaining
// decides; a missing count stays usable.
func githubAPIUsable(client *http.Client, base string) bool {
	resp, err := client.Get(base + "/rate_limit")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Resources struct {
			Core struct {
				Remaining *float64 `json:"remaining"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	return body.Resources.Core.Remaining == nil || *body.Resources.Core.Remaining > 0
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Body       string    `json:"body"`
	Assets     []ghAsset `json:"assets"`
	ZipballURL string    `json:"zipball_url"`
}

// githubAPIError carries the HTTP status from a GitHub API call so callers
// can distinguish 404 (version pin not found) from other failures.
type githubAPIError struct {
	status int
	body   string
}

func (e *githubAPIError) Error() string {
	return fmt.Sprintf("GitHub API Failed(%d) %s", e.status, e.body)
}

// fetchReleaseAsset ports plugin.cljs fetch-release-asset: GET
// {api}/repos/{repo}/{suffix} (5s timeout), pick the first .zip asset,
// falling back to the zipball url for themes. Returns (assetURL, tag,
// releaseNotes). assetURL may be "" when the release has no usable asset.
// Non-200 responses return a *githubAPIError so callers can special-case 404.
func (h *Host) fetchReleaseAsset(repo, suffix string, theme bool) (string, string, string, error) {
	api := h.probeGithubAPIBase()
	endpoint := api + "/repos/" + repo + "/" + suffix
	client := h.proxyClient(5 * time.Second)
	resp, err := client.Get(endpoint)
	if err != nil {
		return "", "", "", fmt.Errorf("[:release-channel-issue %v]", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", "", "", &githubAPIError{status: resp.StatusCode, body: strings.TrimSpace(string(text))}
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", "", fmt.Errorf("[:release-channel-issue %v]", err)
	}
	var assetURL string
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, ".zip") {
			assetURL = a.BrowserDownloadURL
			break
		}
	}
	if assetURL == "" && theme {
		if rel.ZipballURL != "" {
			assetURL = rel.ZipballURL
		} else {
			assetURL = api + "/repos/" + repo + "/zipball"
		}
	}
	return assetURL, rel.TagName, rel.Body, nil
}

// ---- semver helpers (minimal coerce/compare matching semver.js semantics
// for the plugin marketplace's numeric version comparisons) ----

var semverRe = regexp.MustCompile(`(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// semverCoerce extracts the first numeric version core from s and returns
// it normalized to major.minor.patch ("", ok=false when absent).
func semverCoerce(s string) (string, bool) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	parts := []string{m[1], m[2], m[3]}
	for i, p := range parts {
		if p == "" {
			parts[i] = "0"
		}
	}
	return strings.Join(parts, "."), true
}

// semverCompare compares two coerced versions (-1/0/1). Prerelease
// suffixes are ignored (deviation: semver.js orders them before releases).
func semverCompare(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0
		if i < len(as) {
			fmt.Sscanf(as[i], "%d", &av)
		}
		if i < len(bs) {
			fmt.Sscanf(bs[i], "%d", &bv)
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ---- install / update / uninstall ----

// installMarketPluginCmd ports plugin.cljs install-or-update!. The item map
// carries id/repo/version/title/author/description/effect/sponsors/theme/
// plugin-action/only-check. Results are broadcast to all windows as
// "lsp-updates" events with {status, only-check, payload} — Electron's emit
// fans out to every window and each window registers a listener.
func (h *Host) installMarketPluginCmd(w *mygoWindow, item map[string]any) error {
	emitError := func(err error) {
		payload := copyMap(item)
		payload["error-code"] = err.Error()
		h.emitToAll(evLspUpdates, map[string]any{
			"status":     "error",
			"only-check": boolOf(item, "only-check"),
			"payload":    payload,
		})
	}
	emitCompleted := func(payload map[string]any) {
		h.emitToAll(evLspUpdates, map[string]any{
			"status":     "completed",
			"only-check": boolOf(item, "only-check"),
			"payload":    payload,
		})
	}

	repo := stringOf(item, "repo")
	if repo == "" {
		log.Printf("[marketplace] skip install because no repo was given for id %q", stringOf(item, "id"))
		return nil
	}
	repo = strings.TrimSpace(repo)
	if m := reSlashes.FindStringSubmatch(repo); m != nil {
		repo = m[1]
	}

	action := stringOf(item, "plugin-action")
	version := stringOf(item, "version")
	onlyCheck := boolOf(item, "only-check")
	coerced, coercedOK := semverCoerce(version)
	updating := coercedOK && action != "install"

	// Fetch the release: a specific tag for explicit installs, latest otherwise.
	suffix := "releases/latest"
	if action == "install" && version != "" {
		suffix = "releases/tags/" + version
	}
	assetURL, latest, notes, err := h.fetchReleaseAsset(repo, suffix, boolOf(item, "theme"))
	if err != nil && action == "install" && version != "" {
		// fetch-specific-release-asset falls back to latest on 404 only;
		// rate limits / 5xx / timeouts surface as errors like Electron.
		var ghErr *githubAPIError
		if errors.As(err, &ghErr) && ghErr.status == http.StatusNotFound {
			assetURL, latest, notes, err = h.fetchReleaseAsset(repo, "releases/latest", boolOf(item, "theme"))
		}
	}
	if err != nil {
		emitError(err)
		return nil
	}

	// Update flows require a strictly newer version. Electron wraps the
	// comparison in when-let over (semver.coerce latest-version): an
	// uncoercible latest tag skips the gate and installs anyway.
	if updating {
		if coercedLatest, ok := semverCoerce(latest); ok {
			if semverCompare(coerced, coercedLatest) >= 0 {
				// Electron throws (js/Error. :no-new-version); the renderer
				// swallows this exact message.
				emitError(errors.New(":no-new-version"))
				return nil
			}
		}
	}

	if assetURL == "" {
		emitError(fmt.Errorf("[:release-asset-not-found %v]", map[string]any{
			"latest-version": latest,
		}))
		return nil
	}

	dest := filepath.Join(h.PluginsRoot(), stringOf(item, "id"))
	if !onlyCheck {
		if err := h.downloadAssetZip(item, assetURL, latest, dest); err != nil {
			emitError(err)
			return nil
		}
	}

	if onlyCheck {
		payload := copyMap(item)
		payload["latest-version"] = latest
		payload["latest-notes"] = notes
		emitCompleted(payload)
	} else {
		payload := copyMap(item)
		payload["zip"] = assetURL
		payload["dst"] = dest
		payload["installed-version"] = latest
		emitCompleted(payload)
	}
	return nil
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+4)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// downloadAssetZip ports plugin.cljs download-asset-zip: stream dl-url to a
// temp .zip (via .pending), extract beside it, locate the package.json root
// (root or a single wrapped directory), replace <dotdir>/plugins/<id> and
// patch the extracted package.json with the manifest fields.
func (h *Host) downloadAssetZip(item map[string]any, dlURL, dlVersion, dest string) error {
	client := h.proxyClient(30 * time.Second)
	resp, err := client.Get(dlURL)
	if err != nil {
		return fmt.Errorf("[:download-channel-issue %v]", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("[:download-channel-issue %s]", resp.Status)
	}

	basename := path.Base(dlURL)
	if !strings.HasSuffix(basename, ".zip") {
		basename = stringOf(item, "id") + "_" + basename + ".zip"
	}
	tmpPending := filepath.Join(os.TempDir(), basename+".pending")
	tmpZip := strings.TrimSuffix(tmpPending, ".pending")
	extractDir := strings.TrimSuffix(tmpZip, ".zip")

	if err := writeBodyToFile(resp.Body, tmpPending); err != nil {
		return err
	}
	if err := os.Rename(tmpPending, tmpZip); err != nil {
		return err
	}

	if err := os.RemoveAll(extractDir); err != nil {
		return err
	}
	if err := extractZipArchive(tmpZip, extractDir); err != nil {
		_ = os.Remove(tmpZip)
		return err
	}

	root, err := findPluginPackageRoot(extractDir)
	if err != nil {
		_ = os.RemoveAll(extractDir)
		_ = os.Remove(tmpZip)
		return err
	}

	if _, err := os.Stat(dest); err == nil {
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
	}
	if err := movePath(root, dest); err != nil {
		return err
	}
	if err := patchPluginPackageJSON(filepath.Join(dest, "package.json"), item, dlVersion); err != nil {
		return err
	}

	_ = os.RemoveAll(extractDir)
	_ = os.Remove(tmpZip)
	return nil
}

func writeBodyToFile(body io.Reader, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	return f.Close()
}

// extractZipArchive unpacks src into dir, rejecting archive-slip entries.
func extractZipArchive(src, dir string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("illegal plugin archive entry %q", f.Name)
		}
		target := filepath.Join(dir, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeBodyToFile(rc, target)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// findPluginPackageRoot mirrors download-asset-zip's root detection: the
// extraction root itself when it holds package.json, otherwise the last
// (single) wrapped directory holding one.
func findPluginPackageRoot(extractDir string) (string, error) {
	if fileExists(filepath.Join(extractDir, "package.json")) {
		return extractDir, nil
	}
	entries, err := os.ReadDir(extractDir)
	if err != nil {
		return "", err
	}
	root := ""
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(extractDir, e.Name(), "package.json")) {
			root = filepath.Join(extractDir, e.Name())
		} else {
			break
		}
	}
	if root == "" {
		return "", errors.New(":invalid-plugin-package")
	}
	return root, nil
}

// movePath renames src to dst, falling back to a copy for cross-device
// moves (fs.moveSync semantics).
func movePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyDir(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// patchPluginPackageJSON writes the marketplace manifest fields into the
// installed package.json (plugin.cljs download-asset-zip tail).
func patchPluginPackageJSON(pkgPath string, item map[string]any, dlVersion string) error {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return err
	}
	var pkg map[string]any
	if err := json.Unmarshal(data, &pkg); err != nil {
		return err
	}
	if pkg == nil {
		pkg = map[string]any{}
	}
	pkg["repo"] = stringOf(item, "repo")
	pkg["title"] = stringOf(item, "title")
	pkg["author"] = stringOf(item, "author")
	pkg["description"] = stringOf(item, "description")
	pkg["effect"] = boolOf(item, "effect")
	if dlVersion != "" {
		pkg["version"] = dlVersion
	}
	if sponsors, ok := item["sponsors"]; ok && sponsors != nil {
		pkg["sponsors"] = sponsors
	}
	out, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pkgPath, out, 0o644)
}

// uninstallMarketPluginCmd ports plugin.cljs uninstall!: strip leading
// path segments from id, remove plugins/<id> and settings/<id>.json.
func (h *Host) uninstallMarketPluginCmd(id string) error {
	id = reLeadingDot.ReplaceAllString(id, "")
	if id == "" {
		return nil
	}
	pluginPath := filepath.Join(h.PluginsRoot(), id)
	settingsPath := filepath.Join(h.DotRoot(), "settings", id+".json")
	log.Printf("[marketplace] uninstall %s", pluginPath)
	// Electron guards with fs/pathExistsSync, true for directories too —
	// plugin installs are directories, so a plain stat check is required
	// (fileExists excludes directories).
	if _, err := os.Stat(pluginPath); err == nil {
		if err := os.RemoveAll(pluginPath); err != nil {
			return err
		}
		_ = os.Remove(settingsPath)
	}
	return nil
}
