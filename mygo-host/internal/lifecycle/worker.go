package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// target is a discovered worker: the state.json registration record merged
// with the runtime-<ticket>.json publication (port) and probe results.
type target struct {
	// registration record fields
	Ticket            string `json:"ticket"`
	Generation        string `json:"generation"`
	PID               int    `json:"pid"`
	Owner             string `json:"owner"`
	Root              string `json:"root"`
	GraphsDir         string `json:"graphsDir"`
	LifecycleDir      string `json:"lifecycleDir"`
	Repo              string `json:"repo"`
	OwnershipProtocol string `json:"ownership-protocol"`
	// spawn bookkeeping (pending startup workers)
	Parent  int    `json:"parent,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Error   any    `json:"error,omitempty"`
	// publication + probe
	Port        int            `json:"port,omitempty"`
	HealthValue map[string]any `json:"-"`
}

func runtimeRecord(ctx *GraphCtx, ticket, generation string, pid int, owner string) map[string]any {
	return map[string]any{
		"ticket": ticket, "generation": generation, "pid": pid, "owner": owner,
		"root": ctx.Root, "graphsDir": ctx.GraphsDir, "lifecycleDir": ctx.LifecycleDir,
		"repo": ctx.Repo, "ownership-protocol": Protocol,
	}
}

func runtimeRecordFromRecord(record map[string]any) map[string]any {
	keys := []string{"ticket", "generation", "pid", "owner", "root", "graphsDir", "lifecycleDir", "repo", "ownership-protocol"}
	out := map[string]any{}
	for _, k := range keys {
		out[k] = record[k]
	}
	return out
}

var ticketRe = regexp.MustCompile(`^[\w-]+$`)

func runtimeFile(ctx *GraphCtx, ticket string) (string, error) {
	if !ticketRe.MatchString(ticket) {
		return "", fail("Invalid admission ticket")
	}
	return filepath.Join(ctx.Dir, "runtime-"+ticket+".json"), nil
}

func isPending(t *target) bool {
	return t.Phase == "" && t.Expires > time.Now().UnixMilli() && pidExists(t.Parent)
}

func registered(current map[string]any, rec map[string]any) bool {
	workers, _ := current["workers"].([]any)
	for _, w := range workers {
		record, ok := w.(map[string]any)
		if !ok {
			continue
		}
		match := true
		for key, value := range rec {
			if record[key] != value {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func checkAdmission(ctx *GraphCtx, record map[string]any) error {
	current, err := readJSON(ctx.StateFile)
	if err != nil {
		return err
	}
	if err := requireAvailable(ctx, current, stringOf(record["generation"])); err != nil {
		return err
	}
	if !registered(current, record) {
		return coded("server-start-failed", "Worker admission registration changed")
	}
	return nil
}

func validateRegistration(ctx *GraphCtx, current map[string]any, record map[string]any) error {
	pid := intOf(record["pid"])
	ticket := stringOf(record["ticket"])
	owner := stringOf(record["owner"])
	root := stringOf(record["root"])
	generation := stringOf(record["generation"])
	repo := stringOf(record["repo"])
	protocol := stringOf(record["ownership-protocol"])
	graphsDir := stringOf(record["graphsDir"])
	lifecycleDir := stringOf(record["lifecycleDir"])
	if pid <= 0 || ticket == "" || owner == "" || root == "" ||
		generation != stringOf(current["generation"]) || repo != ctx.Repo ||
		protocol != Protocol || graphsDir != ctx.GraphsDir || lifecycleDir != ctx.LifecycleDir {
		return fail("Invalid worker registration")
	}
	return nil
}

// readRuntime mirrors lifecycle readRuntime.
func readRuntime(ctx *GraphCtx, record map[string]any) (map[string]any, error) {
	ticket := stringOf(record["ticket"])
	file, err := runtimeFile(ctx, ticket)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		runtime, err := readJSON(file)
		if err != nil {
			// malformed JSON: JS treats a non-object as invalid metadata
			if attempt < 3 {
				continue
			}
			return nil, fail("Invalid worker runtime metadata")
		}
		if runtime == nil {
			if _, statErr := os.Stat(file); statErr == nil {
				// empty read is unstable mid-write; confirm it
				if attempt < 3 {
					continue
				}
				return nil, fail("Invalid worker runtime metadata")
			}
			return nil, nil
		}
		for key, value := range runtimeRecordFromRecord(record) {
			if runtime[key] != value {
				return nil, fail("Worker runtime identity differs from registration")
			}
		}
		return runtime, nil
	}
}

// request is the 1s-timeout loopback request used for health/shutdown probes.
func request(port int, endpoint, method string, headers map[string]string) (int, string, error) {
	req, err := http.NewRequestWithContext(context.Background(), method,
		fmt.Sprintf("http://127.0.0.1:%d%s", port, endpoint), nil)
	if err != nil {
		return 0, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 1000 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}

// health mirrors lifecycle health(): probe /healthz and verify identity.
func health(ctx *GraphCtx, t *target, port int) (map[string]any, error) {
	status, body, err := request(port, "/healthz", "GET", nil)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		return nil, err
	}
	storageOK := false
	if storage, ok := value["storage"].(map[string]any); ok {
		storageOK = sameStorage(ctx, storage)
	}
	ok := (status == 200 || status == 503) &&
		intOf(value["pid"]) == t.PID &&
		intOf(value["port"]) == port &&
		storageOK &&
		graphNameEquals(value["repo"], ctx.Repo) &&
		stringOf(value["root-dir"]) != "" && canonicalRoot(stringOf(value["root-dir"])) == canonicalRoot(t.Root) &&
		stringOf(value["revision"]) != "" &&
		stringOf(value["host"]) == "127.0.0.1" &&
		stringOf(value["ticket"]) == t.Ticket &&
		stringOf(value["generation"]) == t.Generation &&
		stringOf(value["owner-source"]) == t.Owner &&
		t.OwnershipProtocol == Protocol &&
		stringOf(value["ownership-protocol"]) == Protocol
	if !ok {
		return nil, fail("Worker endpoint identity mismatch")
	}
	return value, nil
}

func graphNameEquals(repoValue any, want string) bool {
	name, err := GraphName(stringOf(repoValue))
	return err == nil && name == want
}

func ignorableDiscoveryError(err error) bool {
	if err == nil {
		return true
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		msg == "Worker request timeout" ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "Client.Timeout")
}

// publication is one <root>/server-list line.
type publication struct {
	pid  int
	port int
}

// entries reads the <root>/server-list file ("pid port" per line).
func entries(root string) ([]publication, error) {
	data, err := os.ReadFile(filepath.Join(root, "server-list"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := []publication{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var pid, port int
		if _, err := fmt.Sscanf(line, "%d %d", &pid, &port); err != nil || pid <= 0 || port <= 0 {
			return nil, fail("Invalid server publication")
		}
		out = append(out, publication{pid, port})
	}
	return out, nil
}

// discover mirrors lifecycle discover: state.json workers -> targets, plus
// server-list publication attach and unregistered-published-worker checks.
func discover(ctx *GraphCtx, current map[string]any) ([]*target, error) {
	workers, _ := current["workers"].([]any)
	targets := map[int]*target{}
	order := []*target{}
	for _, w := range workers {
		record, ok := w.(map[string]any)
		if !ok {
			return nil, fail("Invalid worker registration")
		}
		if err := validateRegistration(ctx, current, record); err != nil {
			return nil, err
		}
		pid := intOf(record["pid"])
		if _, dup := targets[pid]; dup {
			return nil, fail("Duplicate worker registration")
		}
		runtime, err := readRuntime(ctx, record)
		if err != nil {
			return nil, err
		}
		t := &target{
			Ticket: stringOf(record["ticket"]), Generation: stringOf(record["generation"]),
			PID: pid, Owner: stringOf(record["owner"]),
			Root: stringOf(record["root"]), GraphsDir: stringOf(record["graphsDir"]),
			LifecycleDir: stringOf(record["lifecycleDir"]), Repo: stringOf(record["repo"]),
			OwnershipProtocol: stringOf(record["ownership-protocol"]),
			Parent:            intOf(record["parent"]), Expires: int64Of(record["expires"]),
			Phase: stringOf(record["phase"]),
		}
		if runtime != nil {
			t.Port = intOf(runtime["port"])
			if runtime["phase"] != nil {
				t.Phase = stringOf(runtime["phase"])
			}
		}
		targets[pid] = t
		order = append(order, t)
	}
	// server-list publications for this root and any worker's root
	roots := map[string]bool{ctx.Root: true}
	for _, t := range order {
		if t.Root != "" {
			roots[t.Root] = true
		}
	}
	var unregisteredErrs []error
	for root := range roots {
		publications, err := entries(root)
		if err != nil {
			return nil, err
		}
		for _, e := range publications {
			if !pidExists(e.pid) {
				continue
			}
			if t, ok := targets[e.pid]; ok {
				if t.Port != 0 && t.Port != e.port {
					return nil, fail("Worker publication identity mismatch")
				}
				t.Port = e.port
				continue
			}
			// published but unregistered: probe and fail if it is our graph
			_, body, err := request(e.port, "/healthz", "GET", nil)
			if err != nil {
				if ignorableDiscoveryError(err) {
					continue
				}
				return nil, err
			}
			var value map[string]any
			if err := json.Unmarshal([]byte(body), &value); err != nil {
				if ignorableDiscoveryError(err) {
					continue
				}
				return nil, err
			}
			if value != nil && value["repo"] != nil && graphNameEquals(value["repo"], ctx.Repo) {
				if storage, ok := value["storage"].(map[string]any); ok && sameStorage(ctx, storage) {
					unregisteredErrs = append(unregisteredErrs, fail("Published graph worker is unregistered"))
				} else if rootDir := stringOf(value["root-dir"]); rootDir == "" || canonicalRoot(rootDir) == ctx.Root {
					unregisteredErrs = append(unregisteredErrs, fail("Published graph worker is unregistered"))
				}
			}
		}
	}
	if len(unregisteredErrs) > 0 {
		return nil, unregisteredErrs[0]
	}
	return order, nil
}

// removeEntries mirrors lifecycle removeEntries: lock server-list.lock,
// rewrite the file without removed entries, unlock.
func removeEntries(root string, targets []*target) error {
	lockFile := filepath.Join(root, "server-list.lock")
	lockID := id()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(mustJSON(map[string]any{"pid": os.Getpid(), "lock-id": lockID}))
			f.Close()
			break
		}
		if !os.IsExist(err) {
			return err
		}
		if previous, err := readJSON(lockFile); err == nil && previous != nil {
			if !pidExists(intOf(previous["pid"])) {
				if again, err := readJSON(lockFile); err == nil && again != nil && stringOf(again["lock-id"]) == stringOf(previous["lock-id"]) {
					_ = os.Remove(lockFile)
				}
			}
		}
		if time.Now().After(deadline) {
			return fail("Timed out acquiring server-list lock")
		}
		time.Sleep(25 * time.Millisecond)
	}
	defer func() {
		if current, err := readJSON(lockFile); err != nil || current == nil || stringOf(current["lock-id"]) != lockID {
			logLifecycleEvent("server-list-lock-ownership-changed", root, "")
			return
		}
		_ = os.Remove(lockFile)
	}()
	existing, err := entries(root)
	if err != nil {
		return err
	}
	retained := []publication{}
	for _, e := range existing {
		remove := false
		for _, t := range targets {
			if t.PID == e.pid && t.Port == e.port {
				remove = true
				break
			}
		}
		if !remove {
			retained = append(retained, e)
		}
	}
	file := filepath.Join(root, "server-list")
	tmp := file + "." + id() + ".tmp"
	var sb strings.Builder
	for _, e := range retained {
		sb.WriteString(fmt.Sprintf("%d %d\n", e.pid, e.port))
	}
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, file)
}

func waitExit(pid int, milliseconds int) bool {
	deadline := time.Now().Add(time.Duration(milliseconds) * time.Millisecond)
	for {
		if !pidExists(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
		if time.Now().After(deadline) {
			return !pidExists(pid)
		}
	}
}

// children tracks workers spawned by this process (JS `children` map).
var (
	childrenMu sync.Mutex
	children   = map[string]*os.Process{}
)

func trackChild(ticket string, p *os.Process) {
	childrenMu.Lock()
	children[ticket] = p
	childrenMu.Unlock()
}

func untrackChild(ticket string) {
	childrenMu.Lock()
	delete(children, ticket)
	childrenMu.Unlock()
}

func signalTrackedChild(ticket string, pid int, sig syscall.Signal) bool {
	childrenMu.Lock()
	p := children[ticket]
	childrenMu.Unlock()
	if p == nil || p.Pid != pid {
		return false
	}
	if sig == syscall.SIGKILL {
		_ = p.Kill()
	} else {
		_ = p.Signal(sig)
	}
	return true
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func stringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	default:
		return 0
	}
}

func int64Of(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		i, _ := t.Int64()
		return i
	default:
		return 0
	}
}
