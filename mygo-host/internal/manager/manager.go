// Package manager ports src/electron/electron/db_worker.cljs: the
// per-repo db-worker runtime registry with window attachment.
package manager

import (
	"fmt"
	"sync"

	"logseq/mygo-host/internal/daemon"
	"logseq/mygo-host/internal/lifecycle"
	"logseq/mygo-host/internal/lqgraph"
)

// Config carries host-level daemon orchestration settings.
type Config struct {
	RootDir          string
	GraphsDir        string
	Storage          *lifecycle.Storage
	ScriptPath       string
	NodeBin          string
	ExpectedRevision string
}

// OnGraphLifecycle is invoked with (repo, payload) whenever a managed
// graph's lifecycle state changes; the host forwards it to all windows.
type OnGraphLifecycle func(repo string, payload map[string]any)

// Manager mirrors db-worker state {repos, window→repo, epochs}.
type Manager struct {
	mu      sync.Mutex
	cfg     Config
	notify  OnGraphLifecycle
	repos   map[string]*repoEntry // repoKey → entry
	windows map[int]string        // windowID → repoKey
	epochs  map[string]int
}

type repoEntry struct {
	Runtime  *daemon.Runtime
	Windows  map[int]bool
	Stopping bool
	Observer func()
}

// NewManager creates an empty manager.
func NewManager(cfg Config, notify OnGraphLifecycle) *Manager {
	return &Manager{
		cfg:     cfg,
		notify:  notify,
		repos:   map[string]*repoEntry{},
		windows: map[int]string{},
		epochs:  map[string]int{},
	}
}

func repoKey(repo string) string {
	return lqgraph.RepoIdentity(lqgraph.CanonicalRepo(repo))
}

// EnsureStarted mirrors ensure-started!: returns the runtime for repo,
// starting or reusing the daemon as needed.
func (m *Manager) EnsureStarted(repo string, windowID int, generation string) (*daemon.Runtime, error) {
	key := repoKey(repo)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureStartedLocked(repo, key, windowID, generation)
}

func (m *Manager) ensureStartedLocked(repo, key string, windowID int, generation string) (*daemon.Runtime, error) {
	epoch := m.epochs[key]
	// Window switching away from its current repo stops that daemon first.
	if current, ok := m.windows[windowID]; ok && current != key {
		if err := m.ensureWindowStoppedLocked(windowID); err != nil {
			return nil, err
		}
	}
	if entry, ok := m.repos[key]; ok && entry != nil {
		if entry.Stopping {
			// stops are synchronous; a lingering flag means an earlier
			// stop failed and the entry is stale
			m.epochs[key]++
			delete(m.repos, key)
		} else {
			rt := entry.Runtime
			if generation != "" && rt != nil && generation != rt.Generation {
				return nil, &lifecycle.Error{Code: "graph-not-exists", Message: "Graph generation changed"}
			}
			if rt != nil && daemon.RuntimeReadyDefault(rt) {
				entry.Windows[windowID] = true
				m.windows[windowID] = key
				return rt, nil
			}
			// not ready: close observer, stop owned daemon, restart
			m.epochs[key]++
			if entry.Observer != nil {
				entry.Observer()
			}
			if rt != nil && rt.Owned {
				if result := m.stopDaemon(repo); result.OK {
					delete(m.repos, key)
				} else {
					// keep entry; restart below replaces it
				}
			}
			if m.epochs[key] != epoch {
				return nil, fmt.Errorf("worker state changed during restart")
			}
			rt, err := m.startDaemonLocked(repo, key, windowID, generation)
			if err != nil {
				return nil, err
			}
			return rt, nil
		}
	}
	if m.epochs[key] != epoch {
		return nil, fmt.Errorf("worker state changed during start")
	}
	return m.startDaemonLocked(repo, key, windowID, generation)
}

func (m *Manager) startDaemonLocked(repo, key string, windowID int, generation string) (*daemon.Runtime, error) {
	cfg := daemon.Config{
		RootDir:          m.cfg.RootDir,
		GraphsDir:        m.cfg.GraphsDir,
		Storage:          m.cfg.Storage,
		OwnerSource:      "electron",
		Generation:       generation,
		ExpectedRevision: m.cfg.ExpectedRevision,
		ScriptPath:       m.cfg.ScriptPath,
		NodeBin:          m.cfg.NodeBin,
	}
	server, err := daemon.EnsureServer(cfg, repo)
	if err != nil {
		return nil, err
	}
	storage := cfg.Storage
	if storage == nil {
		storage, err = daemon.ResolveStorage(cfg)
		if err != nil {
			return nil, err
		}
	}
	rt := &daemon.Runtime{
		Repo:       lqgraph.CanonicalRepo(repo),
		RootDir:    server.RootDir,
		Generation: server.Generation,
		BaseURL:    server.BaseURL(),
		AuthToken:  nil,
		Owned:      server.IsOwned(),
		Storage:    storage,
	}
	m.installLocked(key, rt, windowID)
	// observe lifecycle changes for this repo
	stop := lifecycle.Observe(storage, rt.Repo, rt.Generation, func(current map[string]any) {
		m.onObservedChange(key, rt.Generation, current)
	})
	if entry, ok := m.repos[key]; ok {
		entry.Observer = stop
	} else {
		stop() // runtime was replaced meanwhile
	}
	return rt, nil
}

func (m *Manager) installLocked(key string, rt *daemon.Runtime, windowID int) {
	entry := m.repos[key]
	if entry == nil {
		entry = &repoEntry{Windows: map[int]bool{}}
		m.repos[key] = entry
	}
	if entry.Runtime != rt && entry.Observer != nil {
		entry.Observer()
		entry.Observer = nil
	}
	entry.Runtime = rt
	entry.Stopping = false
	if windowID > 0 {
		entry.Windows[windowID] = true
		m.windows[windowID] = key
	}
}

// onObservedChange mirrors the db-worker observer callback: invalidate the
// repo (keeping the observer) and notify windows.
func (m *Manager) onObservedChange(key, generation string, current map[string]any) {
	m.mu.Lock()
	entry := m.repos[key]
	if entry != nil && entry.Runtime != nil && entry.Runtime.Generation == generation {
		m.epochs[key]++
		delete(m.repos, key)
		for win, owner := range m.windows {
			if owner == key {
				delete(m.windows, win)
			}
		}
	}
	m.mu.Unlock()
	if m.notify != nil {
		repo := stringOf(current["repo"])
		payload := map[string]any{
			"repo":       repo,
			"phase":      stringOf(current["phase"]),
			"generation": stringOf(current["generation"]),
		}
		if payload["repo"] == "" {
			payload["repo"] = key
		}
		m.notify(payload["repo"].(string), payload)
	}
}

// EnsureWindowStopped mirrors ensure-window-stopped!.
func (m *Manager) EnsureWindowStopped(windowID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureWindowStoppedLocked(windowID)
}

func (m *Manager) ensureWindowStoppedLocked(windowID int) error {
	key, ok := m.windows[windowID]
	if ok {
		delete(m.windows, windowID)
	}
	entry := m.repos[key]
	if entry != nil {
		delete(entry.Windows, windowID)
		if len(entry.Windows) == 0 && entry.Runtime != nil {
			entry.Stopping = true
			err := m.stopEntryLocked(key, entry)
			entry.Stopping = false
			return err
		}
	}
	return nil
}

func (m *Manager) stopEntryLocked(key string, entry *repoEntry) error {
	rt := entry.Runtime
	if entry.Observer != nil {
		entry.Observer()
		entry.Observer = nil
	}
	if rt != nil && rt.Owned {
		if result := m.stopDaemon(rt.Repo); !result.OK {
			return fmt.Errorf("worker stop did not complete: %s: %s", result.Code, result.Message)
		}
	}
	m.epochs[key]++
	delete(m.repos, key)
	for win, owner := range m.windows {
		if owner == key {
			delete(m.windows, win)
		}
	}
	return nil
}

func (m *Manager) stopDaemon(repo string) daemon.StopResult {
	cfg := daemon.Config{
		RootDir:     m.cfg.RootDir,
		GraphsDir:   m.cfg.GraphsDir,
		Storage:     m.cfg.Storage,
		OwnerSource: "electron",
	}
	return daemon.StopServer(cfg, repo)
}

// InvalidateRepo mirrors invalidate-repo!: drop cached runtime state and
// bump the epoch so in-flight operations abort.
func (m *Manager) InvalidateRepo(repo string) {
	key := repoKey(repo)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epochs[key]++
	if entry, ok := m.repos[key]; ok {
		if entry.Observer != nil {
			entry.Observer()
			entry.Observer = nil
		}
		delete(m.repos, key)
	}
	for win, owner := range m.windows {
		if owner == key {
			delete(m.windows, win)
		}
	}
}

// ReleaseRuntime mirrors ensure-stopped! (release-runtime!): stop the
// daemon for a repo regardless of windows.
func (m *Manager) ReleaseRuntime(repo string) error {
	key := repoKey(repo)
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.repos[key]
	if entry == nil {
		return nil
	}
	return m.stopEntryLocked(key, entry)
}

// ReleaseWindow mirrors release-window!.
func (m *Manager) ReleaseWindow(windowID int) { _ = m.EnsureWindowStopped(windowID) }

// StopAll mirrors stop-all-db-workers.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, entry := range m.repos {
		if entry.Runtime != nil && entry.Runtime.Owned {
			_ = m.stopDaemon(entry.Runtime.Repo)
		}
		if entry.Observer != nil {
			entry.Observer()
		}
		m.epochs[key]++
		delete(m.repos, key)
	}
	m.windows = map[int]string{}
}

// RuntimeFor returns the current runtime for a repo (nil when absent).
func (m *Manager) RuntimeFor(repo string) *daemon.Runtime {
	key := repoKey(repo)
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry := m.repos[key]; entry != nil {
		return entry.Runtime
	}
	return nil
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}
