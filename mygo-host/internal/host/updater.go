package host

// Updater — port of Electron src/electron/electron/updater.cljs for a host
// without electron-updater: "check-for-updates" pushes "updates-callback"
// events with the same type/payload shapes the renderer's
// setUpdatesCallback listener expects. Two feed modes:
//
//   - GitHub-release mode (default): fetches the latest logseq/logseq
//     GitHub release (LOGSEQ_UPDATES_URL overrides the endpoint).
//   - Manifest mode (LOGSEQ_UPDATE_PUBLIC_KEY set): LOGSEQ_UPDATES_URL
//     points at a mygo update-<target>.json manifest; the fetch is
//     Ed25519-verified (manifest signature over its fields) before any
//     update is announced.
//
// "install-updates" cannot self-replace a running binary — it opens the
// release/download page and logs the limitation (documented deviation).

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"logseq/mygo-host/internal/updatemanifest"
)

const (
	logseqReleasesRepo  = "logseq/logseq"
	logseqReleasesPage  = "https://github.com/logseq/logseq/releases/latest"
	updaterFetchTimeout = 5 * time.Second
)

var (
	updatePendingMu  sync.Mutex
	updatePending    bool
	downloadedUpdate any // always nil: the host never downloads updates
)

// appVersion resolves the running version the same way Electron's
// frontend.version does (package.json version baked at build time):
// LOGSEQ_VERSION env / mygo.json version, defaulting to 0.0.1.
func appVersion() string {
	if v := mygoApp().Version(); v != "" {
		return v
	}
	return "0.0.1"
}

type updaterInfo struct {
	Version      string `json:"version"`
	Name         string `json:"name"`
	ReleaseName  string `json:"releaseName"`
	ReleaseNotes string `json:"releaseNotes"`
	ReleaseDate  string `json:"releaseDate"`
	URL          string `json:"url"`
}

func (h *Host) emitUpdate(w *mygoWindow, typ string, payload any) {
	log.Printf("[host] updates-callback emit type=%s win=%v", typ, w != nil)
	if w != nil && !w.IsDestroyed() {
		evUpdates.Emit(w, map[string]any{"type": typ, "payload": payload})
	} else {
		evUpdates.Broadcast(map[string]any{"type": typ, "payload": payload})
	}
}

func (h *Host) emitUpdateCompleted(w *mygoWindow) {
	h.emitUpdate(w, "completed", nil)
}

// checkForUpdates ports <check-for-updates! + the autoUpdater event
// handlers: checking-for-update → (update-available | update-not-available
// | error) → completed. The pending guard mirrors *update-pending.
func (h *Host) checkForUpdates(w *mygoWindow, autoDownload bool) {
	updatePendingMu.Lock()
	if updatePending {
		updatePendingMu.Unlock()
		return
	}
	updatePending = true
	updatePendingMu.Unlock()
	defer func() {
		updatePendingMu.Lock()
		updatePending = false
		updatePendingMu.Unlock()
	}()

	log.Printf("[updater] check-for-updates auto-download=%v", autoDownload)
	h.emitUpdate(w, "checking-for-update", nil)

	var info *updaterInfo
	var err error
	if updaterManifestMode() {
		info, err = h.fetchLatestManifestUpdate()
	} else {
		info, err = h.fetchLatestLogseqRelease()
	}
	if err != nil {
		log.Printf("[updater/check] %v", err)
		h.emitUpdate(w, "error", map[string]any{"message": err.Error()})
		h.emitUpdateCompleted(w)
		return
	}

	latest := info.Version
	current := appVersion()
	coercedLatest, ok := semverCoerce(latest)
	if !ok {
		err := fmt.Errorf("[:invalid-latest-version %q]", latest)
		h.emitUpdate(w, "error", map[string]any{"message": err.Error()})
		h.emitUpdateCompleted(w)
		return
	}
	coercedCurrent, _ := semverCoerce(current)
	if semverCompare(coercedCurrent, coercedLatest) < 0 {
		h.emitUpdate(w, "update-available", info)
	} else {
		h.emitUpdate(w, "update-not-available", map[string]any{"version": current})
	}
	// Terminal event for manual checks (updater.cljs <check-for-updates!
	// then-branch). Auto-download runs never download, so every path ends
	// here in the host.
	h.emitUpdateCompleted(w)
}

// fetchLatestLogseqRelease resolves the endpoint (LOGSEQ_UPDATES_URL or the
// GitHub latest-release API, proxy-aware like the marketplace fetcher) and
// normalizes the release JSON into updaterInfo.
func (h *Host) fetchLatestLogseqRelease() (*updaterInfo, error) {
	endpoint := os.Getenv("LOGSEQ_UPDATES_URL")
	if endpoint == "" {
		endpoint = h.probeGithubAPIBase() + "/repos/" + logseqReleasesRepo + "/releases/latest"
	}
	client := h.proxyClient(updaterFetchTimeout)
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("[:release-channel-issue %v]", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("GitHub API Failed(%d) %s", resp.StatusCode, strings.TrimSpace(string(text)))
	}
	var rel struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		Body        string `json:"body"`
		PublishedAt string `json:"published_at"`
		HtmlURL     string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("[:release-channel-issue %v]", err)
	}
	name := rel.Name
	if name == "" {
		name = rel.TagName
	}
	return &updaterInfo{
		Version:      rel.TagName,
		Name:         name,
		ReleaseName:  name,
		ReleaseNotes: rel.Body,
		ReleaseDate:  rel.PublishedAt,
		URL:          rel.HtmlURL,
	}, nil
}

// installUpdatesAndQuitApp ports the install-updates channel
// (autoUpdater.quitAndInstall(false, true) in Electron). The mygo updater
// plugin is not adopted for in-app apply/relaunch: it is a self-driving
// UI (own menu item + prefs + auto checks) that requires the update feed
// and public key baked into the build config, while this host's update UX
// is renderer-driven over updates-callback. A Go host also cannot
// self-replace its running binary: log the limitation and open the
// release/download page so the user can install manually. The app is NOT
// quit (documented deviation).
func (h *Host) installUpdatesAndQuitApp(w *mygoWindow, quitApp bool) {
	page := logseqReleasesPage
	if info, err := h.fetchLatestLogseqRelease(); err == nil && info.URL != "" {
		page = info.URL
	}
	if updaterManifestMode() {
		if info, err := h.fetchLatestManifestUpdate(); err == nil && info.URL != "" {
			page = info.URL
		}
	}
	log.Printf("[updater] install-updates requested (quit=%v): no in-app self-replace in the mygo host — "+
		"opening the update download page for a manual install: %s", quitApp, page)
	if err := mygoShell().OpenExternal(page); err != nil {
		log.Printf("[updater] open release page: %v", err)
	}
}

// updaterManifestMode: the mygo manifest feed is selected when a public
// key for Ed25519 verification is configured.
func updaterManifestMode() bool {
	return os.Getenv("LOGSEQ_UPDATE_PUBLIC_KEY") != ""
}

// updaterPublicKey decodes LOGSEQ_UPDATE_PUBLIC_KEY (base64 Ed25519).
func updaterPublicKey() (ed25519.PublicKey, error) {
	text := strings.TrimSpace(os.Getenv("LOGSEQ_UPDATE_PUBLIC_KEY"))
	if text == "" {
		return nil, fmt.Errorf("LOGSEQ_UPDATE_PUBLIC_KEY is not set")
	}
	key, err := updatemanifest.ParsePublicKey(text)
	if err != nil {
		return nil, fmt.Errorf("LOGSEQ_UPDATE_PUBLIC_KEY: %w", err)
	}
	return key, nil
}

// fetchLatestManifestUpdate is the mygo manifest mode: LOGSEQ_UPDATES_URL
// names an update-<target>.json manifest (mygo docs/updates.md schema);
// the fetch is capped at mygo's manifest size limit and its
// manifestSignature is verified against LOGSEQ_UPDATE_PUBLIC_KEY before
// the version is announced. The archive signature is checked at download
// time by the full updater (same key); the check here only gates the
// announcement.
func (h *Host) fetchLatestManifestUpdate() (*updaterInfo, error) {
	feed := os.Getenv("LOGSEQ_UPDATES_URL")
	if feed == "" {
		return nil, fmt.Errorf("manifest mode needs LOGSEQ_UPDATES_URL pointing at update-<target>.json")
	}
	client := h.proxyClient(updaterFetchTimeout)
	resp, err := client.Get(feed)
	if err != nil {
		return nil, fmt.Errorf("[:release-channel-issue %v]", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("update feed Failed(%d) %s", resp.StatusCode, strings.TrimSpace(string(text)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, updatemanifest.MaxManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf("[:release-channel-issue %v]", err)
	}
	m, err := updatemanifest.DecodeManifest(body)
	if err != nil {
		return nil, fmt.Errorf("[:release-channel-issue %v]", err)
	}
	key, err := updaterPublicKey()
	if err != nil {
		return nil, err
	}
	if err := updatemanifest.VerifyManifest(key, m); err != nil {
		return nil, fmt.Errorf("[:release-channel-issue %v]", err)
	}
	name := "Logseq " + m.Version
	return &updaterInfo{
		Version:      m.Version,
		Name:         name,
		ReleaseName:  name,
		ReleaseNotes: m.Notes,
		ReleaseDate:  m.Date,
		URL:          m.URL,
	}, nil
}
