// Package daemon ports logseq.cli.server: db-worker-node lifecycle
// orchestration shared by the desktop host (owner "electron") and CLI.
package daemon

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"logseq/mygo-host/internal/lifecycle"
	"logseq/mygo-host/internal/lqgraph"
)

// Config mirrors the cli-server config map.
type Config struct {
	RootDir         string
	GraphsDir       string
	Storage         *lifecycle.Storage
	OwnerSource     string // "electron" (desktop host) or "cli"
	Generation      string
	CreateEmptyDB   bool
	EmbeddingEndpoint string
	EmbeddingModelID  string
	ExpectedRevision  string
	ScriptPath      string // db-worker-node.js
	NodeBin         string // node executable used to run the script
}

// Server is a discovered or started db-worker-node.
type Server struct {
	Repo              string `json:"repo"`
	Status            string `json:"status"`
	Host              string `json:"host"`
	Port              int    `json:"port"`
	PID               int    `json:"pid"`
	OwnerSource       string `json:"owner-source"`
	OwnershipProtocol string `json:"ownership-protocol"`
	Ticket            string `json:"ticket"`
	Generation        string `json:"generation"`
	RootDir           string    `json:"root-dir"`
	Revision          string    `json:"revision"`

	baseURL string
	owned   bool
}

// BaseURL returns http://host:port for the worker HTTP API.
func (s *Server) BaseURL() string {
	if s.baseURL != "" {
		return s.baseURL
	}
	return fmt.Sprintf("http://%s:%d", s.Host, s.Port)
}

// IsOwned reports whether the requesting owner may manage this worker.
func (s *Server) IsOwned() bool { return s.owned }

// Runtime is the db-worker runtime handed to the renderer via
// db-worker-runtime IPC.
type Runtime struct {
	Repo          string    `json:"repo"`
	RootDir       string    `json:"root-dir"`
	Generation    string    `json:"generation"`
	BaseURL       string    `json:"base-url"`
	AuthToken     any       `json:"auth-token"`
	Owned         bool      `json:"owned?"`
	Storage       *lifecycle.Storage `json:"-"`
}

// GetDBGraphsDir returns the default DB graphs dir (~/logseq/graphs),
// honoring LOGSEQ_GRAPHS_DIR.
func GetDBGraphsDir() string {
	if env := os.Getenv("LOGSEQ_GRAPHS_DIR"); env != "" {
		return lqgraph.ExpandHome(env)
	}
	return lqgraph.ExpandHome("~/logseq/graphs")
}

// ResolveRootDir mirrors cli.server/resolve-root-dir.
func ResolveRootDir(cfg Config) string {
	root := cfg.RootDir
	if root == "" {
		root = filepath.Dir(GetDBGraphsDir())
	}
	return lqgraph.ExpandHome(root)
}

// GraphsDir mirrors cli.server/graphs-dir.
func GraphsDir(cfg Config) string {
	if cfg.Storage != nil {
		return cfg.Storage.GraphsDir
	}
	if cfg.GraphsDir != "" {
		return cfg.GraphsDir
	}
	if cfg.RootDir != "" {
		return filepath.Join(ResolveRootDir(cfg), "graphs")
	}
	return GetDBGraphsDir()
}

// ResolveStorage mirrors cli.server/resolve-storage.
func ResolveStorage(cfg Config) (*lifecycle.Storage, error) {
	if cfg.Storage != nil {
		return cfg.Storage, nil
	}
	return lifecycle.ResolveStorage(ResolveRootDir(cfg), GraphsDir(cfg))
}

func requesterOwnerSource(cfg Config) string {
	if cfg.OwnerSource == "" {
		return "cli"
	}
	return cfg.OwnerSource
}

func normalizeOwnerSource(owner string) string {
	switch owner {
	case "electron", "cli", "unknown":
		return owner
	case "":
		return "unknown"
	default:
		return owner
	}
}

func ownerManageable(requester, lockOwner string) bool {
	return requester == lockOwner || (requester == "cli" && lockOwner == "unknown")
}

func expectedRevision(cfg Config) string {
	if cfg.ExpectedRevision != "" {
		return cfg.ExpectedRevision
	}
	return ""
}

func revisionMatch(expected, serverRevision string) bool {
	return serverRevision != "" && expected == serverRevision
}

// EnsureServerStartedOnce mirrors ensure-server-started-once!: startGraph
// (reusing any live worker) and derive owned?.
func EnsureServerStartedOnce(cfg Config, repo string) (*Server, error) {
	storage, err := ResolveStorage(cfg)
	if err != nil {
		return nil, err
	}
	extraArgs := []string{}
	if cfg.EmbeddingEndpoint != "" {
		extraArgs = append(extraArgs, "--embedding-endpoint", cfg.EmbeddingEndpoint)
	}
	if cfg.EmbeddingModelID != "" {
		extraArgs = append(extraArgs, "--embedding-model-id", cfg.EmbeddingModelID)
	}
	value, err := lifecycle.StartGraph(lifecycle.StartOptions{
		Storage:     storage,
		Repo:        repo,
		Script:      cfg.ScriptPath,
		NodeBin:     cfg.NodeBin,
		Owner:       requesterOwnerSource(cfg),
		CreateEmpty: cfg.CreateEmptyDB,
		Generation:  cfg.Generation,
		ExtraArgs:   extraArgs,
	})
	if err != nil {
		return nil, err
	}
	owner := normalizeOwnerSource(stringOf(value["owner-source"]))
	return &Server{
		Repo:              stringOf(value["repo"]),
		Status:            stringOf(value["status"]),
		Host:              stringOf(value["host"]),
		Port:              intOf(value["port"]),
		PID:               intOf(value["pid"]),
		OwnerSource:       owner,
		OwnershipProtocol: stringOf(value["ownership-protocol"]),
		Ticket:            stringOf(value["ticket"]),
		Generation:        stringOf(value["generation"]),
		RootDir:           stringOf(value["root-dir"]),
		Revision:          stringOf(value["revision"]),
		owned:             ownerManageable(requesterOwnerSource(cfg), owner),
	}, nil
}

// EnsureServer mirrors ensure-server-started! + ensure-server!: start (or
// reuse) the worker; on revision mismatch stop it cross-owner and retry
// exactly once.
func EnsureServer(cfg Config, repo string) (*Server, error) {
	expected := expectedRevision(cfg)
	server, err := EnsureServerStartedOnce(cfg, repo)
	if err != nil {
		return nil, err
	}
	if expected == "" || revisionMatch(expected, server.Revision) {
		return server, nil
	}
	// Revision mismatch: stop allowing cross-owner, then retry once.
	if result := StopServerTarget(cfg, repo, server.OwnerSource); !result.OK {
		return nil, fmt.Errorf("db-worker-node revision mismatch and restart failed (stop error: %s: %s)",
			result.Code, result.Message)
	}
	server2, err := EnsureServerStartedOnce(cfg, repo)
	if err != nil {
		return nil, err
	}
	if !revisionMatch(expected, server2.Revision) {
		return nil, fmt.Errorf(
			"db-worker-node revision still does not match after restart; db-worker-node path: %s (expected %s, actual %s)",
			cfg.ScriptPath, expected, server2.Revision)
	}
	return server2, nil
}

// StopResult mirrors the stop-server! result map.
type StopResult struct {
	OK      bool
	Code    string
	Message string
}

// StopServerTarget mirrors stop-server-target!: allowCrossOwner stops with
// the target server's own owner source.
func StopServerTarget(cfg Config, repo string, allowCrossOwnerOwner string) StopResult {
	storage, err := ResolveStorage(cfg)
	if err != nil {
		return StopResult{OK: false, Code: "server-stop-failed", Message: err.Error()}
	}
	owner := requesterOwnerSource(cfg)
	if allowCrossOwnerOwner != "" {
		owner = allowCrossOwnerOwner
	}
	if err := lifecycle.StopGraph(storage, repo, owner); err != nil {
		code, message := lifecycle.ErrorParts(err)
		return StopResult{OK: false, Code: code, Message: message}
	}
	return StopResult{OK: true}
}

// StopServer mirrors cli.server/stop-server!.
func StopServer(cfg Config, repo string) StopResult {
	return StopServerTarget(cfg, repo, "")
}

// DiscoverServers mirrors discover-servers: probe server-list entries,
// drop dead pids, keep healthz payloads.
func DiscoverServers(cfg Config) ([]*Server, error) {
	rootDir := ResolveRootDir(cfg)
	entries, err := lifecycle.Entries(rootDir)
	if err != nil {
		return nil, err
	}
	var retained []*Server
	var stale []lifecycle.Publication
	for _, entry := range entries {
		switch lifecycle.PIDStatusOf(entry.PID) {
		case lifecycle.PIDNotFound:
			stale = append(stale, entry)
			continue
		default:
			// alive or no-permission: retain
		}
		server, err := FetchHealthz(entry.Port)
		if err != nil {
			retained = append(retained, &Server{Host: "127.0.0.1", Port: entry.Port, PID: entry.PID})
			continue
		}
		retained = append(retained, server)
	}
	if len(stale) > 0 {
		if err := lifecycle.RemoveEntries(rootDir, stale); err != nil {
			return nil, err
		}
	}
	// Filter to servers matching this config's canonical root dir.
	current := lqgraph.CanonicalizePath(rootDir)
	var out []*Server
	for _, server := range retained {
		if server.RootDir == "" {
			continue
		}
		if lqgraph.CanonicalizePath(server.RootDir) == current {
			out = append(out, server)
		}
	}
	return out, nil
}

// FetchHealthz probes a worker /healthz endpoint.
func FetchHealthz(port int) (*Server, error) {
	client := &http.Client{Timeout: 1000 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return nil, err
	}
	body, err := lifecycle.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	value, err := lifecycle.ParseJSONBody(body)
	if err != nil {
		return nil, err
	}
	return &Server{
		Repo:              stringOf(value["repo"]),
		Status:            stringOf(value["status"]),
		Host:              stringOf(value["host"]),
		Port:              intOf(value["port"]),
		PID:               intOf(value["pid"]),
		OwnerSource:       normalizeOwnerSource(stringOf(value["owner-source"])),
		OwnershipProtocol: stringOf(value["ownership-protocol"]),
		Ticket:            stringOf(value["ticket"]),
		Generation:        stringOf(value["generation"]),
		RootDir:           stringOf(value["root-dir"]),
		Revision:          stringOf(value["revision"]),
	}, nil
}

// RuntimeReadyDefault mirrors db-worker runtime-ready-default?: the
// lifecycle snapshot must match the runtime generation/phase and the
// daemon must answer /healthz with 200.
func RuntimeReadyDefault(rt *Runtime) bool {
	if rt == nil || rt.BaseURL == "" {
		return false
	}
	parsed, err := url.Parse(rt.BaseURL)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	port := 0
	fmt.Sscanf(parsed.Port(), "%d", &port)
	if port <= 0 {
		return false
	}
	current, err := lifecycle.Snapshot(rt.Storage, rt.Repo)
	if err != nil || current == nil {
		return false
	}
	if stringOf(current["generation"]) != rt.Generation || stringOf(current["phase"]) != "available" {
		return false
	}
	server, err := FetchHealthz(port)
	return err == nil && server != nil && server.Status == "ready"
}

// ResolveDaemonScript mirrors db-worker-script-path: prefer
// <resources>/db-worker-node.js, then <resources>/js/db-worker-node.js.
func ResolveDaemonScript(resourcesDir string) (string, error) {
	if resourcesDir == "" {
		return "", fmt.Errorf("resources dir is required")
	}
	candidates := []string{
		filepath.Join(resourcesDir, "db-worker-node.js"),
		filepath.Join(resourcesDir, "js", "db-worker-node.js"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("db-worker-node.js not found under %s (looked in %s); run the repo static build (pnpm gulp) first",
		resourcesDir, strings.Join(candidates, ", "))
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	default:
		return 0
	}
}
