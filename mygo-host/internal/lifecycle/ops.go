package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"logseq/mygo-host/internal/lqgraph"
)

// legacyCheck fails fast on pre-sqlite-v1 artifacts (milestone-1 deviation:
// no legacy shutdown support).
func legacyCheck(ctx *GraphCtx, current map[string]any) error {
	// db-worker.lock in the graph directory
	if _, err := os.Stat(filepath.Join(ctx.GraphDir, "db-worker.lock")); err == nil {
		return coded("legacy-ownership",
			"Legacy ownership unresolved for %s: legacy db-worker.lock present. Close old applications and daemons before explicit offline recovery.", ctx.GraphDir)
	}
	workers, _ := current["workers"].([]any)
	for _, w := range workers {
		record, _ := w.(map[string]any)
		if record == nil {
			continue
		}
		if stringOf(record["ownership-protocol"]) != Protocol {
			return coded("legacy-ownership",
				"Legacy ownership unresolved for %s: worker %v lacks %s registration. Close old applications and daemons before explicit offline recovery.",
				ctx.GraphDir, record["ticket"], Protocol)
		}
	}
	return nil
}

// shutdownAndWait mirrors lifecycle shutdownAndWait.
func shutdownAndWait(ctx *GraphCtx, t *target, deleting bool, responsive bool) error {
	if t.PID == os.Getpid() {
		return errSelfSignal
	}
	if !pidExists(t.PID) {
		return nil
	}
	if responsive && t.Port != 0 {
		headers := map[string]string{}
		if deleting {
			headers["x-logseq-graph-deleting"] = "true"
		}
		status, _, err := request(t.Port, "/v1/shutdown", "POST", headers)
		if err != nil {
			if !ignorableShutdownError(err) {
				return err
			}
		} else if status != 200 {
			return fail("Worker rejected shutdown")
		}
	}
	stages := []struct {
		name string
		sig  syscall.Signal
		ms   int
	}{
		{"graceful", 0, 5000},
		{"SIGTERM", syscall.SIGTERM, 1000},
		{"SIGKILL", syscall.SIGKILL, 2000},
	}
	for _, stage := range stages {
		if stage.sig != 0 {
			_ = signalProcess(t.PID, stage.sig)
		}
		logLifecycleEvent("worker-termination", ctx.Repo, stage.name)
		if waitExit(t.PID, stage.ms) {
			return nil
		}
	}
	return coded("server-stop-timeout", "Timed out stopping worker %d", t.PID)
}

func ignorableShutdownError(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		msg == "Worker request timeout" ||
		strings.Contains(msg, "Client.Timeout")
}

// terminate mirrors lifecycle terminate.
func terminate(ctx *GraphCtx, t *target, deleting bool) error {
	if !pidExists(t.PID) {
		return nil
	}
	if ownershipAvailable(ctx) {
		if t.Port != 0 {
			responsive := false
			_, err := health(ctx, t, t.Port)
			if err == nil {
				responsive = true
			} else if !ignorableDiscoveryError(err) {
				return err
			}
			if responsive {
				return shutdownAndWait(ctx, t, deleting, true)
			}
		}
		waiting := isPending(t)
		if waiting {
			latest, err := State(ctx)
			if err == nil {
				latest["workers"] = filterWorkers(latest, t.Ticket)
				_ = writeJSON(ctx.StateFile, latest)
			}
		}
		if signalTrackedChild(t.Ticket, t.PID, syscall.SIGTERM) {
			if !waitExit(t.PID, 1000) {
				signalTrackedChild(t.Ticket, t.PID, syscall.SIGKILL)
			}
			if !waitExit(t.PID, 2000) {
				return fail("Startup worker did not exit")
			}
		} else if waiting && !waitExit(t.PID, 5000) {
			return coded("server-stop-timeout", "Revoked startup worker did not exit")
		}
		return nil
	}
	if t.PID == os.Getpid() {
		return errSelfSignal
	}
	responsive := false
	if t.Port != 0 {
		_, err := health(ctx, t, t.Port)
		if err == nil {
			responsive = true
		} else if !ignorableDiscoveryError(err) {
			return err
		}
	}
	return shutdownAndWait(ctx, t, deleting, responsive)
}

func filterWorkers(current map[string]any, ticket string) []any {
	workers, _ := current["workers"].([]any)
	out := []any{}
	for _, w := range workers {
		record, _ := w.(map[string]any)
		if record == nil || stringOf(record["ticket"]) != ticket {
			out = append(out, w)
		}
	}
	return out
}

// cleanup mirrors lifecycle cleanup: remove server-list entries, honor
// acknowledged close errors, delete runtime files.
func cleanup(ctx *GraphCtx, current map[string]any, targets []*target) error {
	roots := map[string]bool{ctx.Root: true}
	for _, t := range targets {
		if t.Root != "" {
			roots[t.Root] = true
		}
	}
	rootsList := make([]string, 0, len(roots))
	for root := range roots {
		rootsList = append(rootsList, root)
	}
	sort.Strings(rootsList)
	for _, root := range rootsList {
		if err := removeEntries(root, targets); err != nil {
			return err
		}
	}
	for _, t := range targets {
		file, err := runtimeFile(ctx, t.Ticket)
		if err != nil {
			return err
		}
		if record, err := readJSON(file); err == nil && record != nil {
			if errText := stringOf(record["error"]); errText != "" {
				acknowledged, _ := current["acknowledgedErrors"].([]any)
				has := false
				for _, a := range acknowledged {
					if stringOf(a) == t.Ticket {
						has = true
						break
					}
				}
				if !has {
					current["acknowledgedErrors"] = append(acknowledged, t.Ticket)
					if err := writeJSON(ctx.StateFile, current); err != nil {
						return err
					}
					return fail("Worker close failed: %s", errText)
				}
			}
		}
	}
	for _, t := range targets {
		if file, err := runtimeFile(ctx, t.Ticket); err == nil {
			_ = os.Remove(file)
		}
	}
	return nil
}

// stopUnderLease mirrors lifecycle stopUnderLease.
func stopUnderLease(ctx *GraphCtx, current map[string]any, deleting bool, owner string) (bool, error) {
	if err := legacyCheck(ctx, current); err != nil {
		return false, err
	}
	targets, err := discover(ctx, current)
	if err != nil {
		return false, err
	}
	available := ownershipAvailable(ctx)
	if !deleting {
		for _, t := range targets {
			if !pidExists(t.PID) || (available && !isPending(t)) {
				continue
			}
			source := t.Owner
			if source != owner && !(owner == "cli" && source == "unknown") {
				return false, coded("server-owned-by-other", "Server is owned by another process")
			}
		}
	}
	for _, t := range targets {
		if err := terminate(ctx, t, deleting); err != nil {
			return false, err
		}
	}
	probe, err := acquireOwnership(ctx)
	if err != nil {
		return false, err
	}
	probe.release()
	if err := cleanup(ctx, current, targets); err != nil {
		return false, err
	}
	current["workers"] = []any{}
	if err := writeJSON(ctx.StateFile, current); err != nil {
		return false, err
	}
	return len(targets) > 0, nil
}

// StopGraph mirrors lifecycle stopGraph.
func StopGraph(storage *Storage, repo, owner string) error {
	ctx, err := Context(storage, repo)
	if err != nil {
		return err
	}
	_, err = withLease(ctx, "stop", func(current map[string]any) (any, error) {
		stopped, err := stopUnderLease(ctx, current, false, owner)
		if err != nil {
			return nil, err
		}
		if !stopped {
			return nil, coded("server-not-found", "Server is not running")
		}
		return map[string]any{"repo": repo}, nil
	})
	return err
}

// CreateGraph mirrors lifecycle createGraph; returns the generation.
func CreateGraph(storage *Storage, repo string) (string, error) {
	ctx, err := Context(storage, repo)
	if err != nil {
		return "", err
	}
	result, err := withLease(ctx, "create", func(current map[string]any) (any, error) {
		phase, _ := current["phase"].(string)
		if _, statErr := os.Stat(ctx.GraphDir); phase == "available" && statErr == nil {
			return stringOf(current["generation"]), nil
		}
		if deletion, ok := current["deletion"].(map[string]any); ok {
			moved, _ := deletion["moved"].(bool)
			if !moved {
				if _, statErr := os.Stat(ctx.GraphDir); statErr == nil {
					return "", fail("Graph deletion must finish before recreation")
				}
			}
		}
		if _, err := stopUnderLease(ctx, current, true, ""); err != nil {
			return nil, err
		}
		handle, err := acquireOwnership(ctx)
		if err != nil {
			return nil, err
		}
		defer handle.release()
		if err := os.MkdirAll(ctx.GraphDir, 0o755); err != nil {
			return nil, err
		}
		next := map[string]any{
			"generation": id(), "phase": "available", "workers": []any{},
			"owner": current["owner"],
		}
		if err := writeJSON(ctx.StateFile, next); err != nil {
			return nil, err
		}
		return stringOf(next["generation"]), nil
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

// directoryIdentity ports fs.statSync {dev, ino}.
func directoryIdentity(path string) (dev uint64, ino uint64, ok bool, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return 0, 0, false, nil
		}
		return 0, 0, false, statErr
	}
	return devOf(info), inoOf(info), true, nil
}

// DeleteGraph mirrors lifecycle deleteGraph; commit is unused by the
// desktop host (electron passes nil).
func DeleteGraph(storage *Storage, repo string) (existed bool, destination string, err error) {
	ctx, err2 := Context(storage, repo)
	if err2 != nil {
		return false, "", err2
	}
	observed, err2 := Snapshot(storage, repo)
	if err2 != nil {
		return false, "", err2
	}
	result, err2 := withLease(ctx, "delete", func(current map[string]any) (any, error) {
		if observed != nil && stringOf(observed["generation"]) != stringOf(current["generation"]) {
			return nil, coded("graph-not-exists", "Graph generation changed")
		}
		observedDeletion, _ := observed["deletion"].(map[string]any)
		currentDeletion, _ := current["deletion"].(map[string]any)
		if observedDeletion != nil && stringOf(observedDeletion["id"]) != stringOf(currentDeletion["id"]) {
			return nil, coded("graph-not-exists", "Graph deletion operation changed")
		}
		if stringOf(current["phase"]) == "deleted" {
			return map[string]any{"existed": false, "destination": nil}, nil
		}
		if currentDeletion == nil {
			currentDeletion = map[string]any{"id": id(), "generation": current["generation"], "moved": false}
			current["deletion"] = currentDeletion
		}
		operation := currentDeletion
		if stringOf(operation["generation"]) != stringOf(current["generation"]) {
			return nil, coded("graph-not-exists", "Graph deletion generation changed")
		}
		current["phase"] = "deleting"
		if err := writeJSON(ctx.StateFile, current); err != nil {
			return nil, err
		}
		mutationOwnership, ownErr := acquireOwnership(ctx)
		if ownErr != nil {
			current["phase"] = "deletion-failed"
			current["error"] = ownErr.Error()
			_ = writeJSON(ctx.StateFile, current)
			return nil, ownErr
		}
		defer mutationOwnership.release()
		existed := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			var stopErr error
			_, stopErr = stopUnderLease(ctx, current, true, "")
			if stopErr != nil {
				err = stopErr
				return
			}
			sDev, sIno, sOK, sErr := directoryIdentity(ctx.GraphDir)
			if sErr != nil {
				err = sErr
				return
			}
			destPath := stringOf(operation["destination"])
			if destPath != "" && operation["moved"] != true {
				// crash recovery: verify the move already happened
				dDev, dIno, dOK, dErr := directoryIdentity(destPath)
				if dErr != nil {
					err = dErr
					return
				}
				opSourceDev, opSourceIno := uint64Of(operation["source.dev"]), uint64Of(operation["source.ino"])
				if !sOK && dOK && opSourceDev == dDev && opSourceIno == dIno {
					operation["moved"] = true
				} else if opSourceDev != sDev || opSourceIno != sIno || dOK {
					err = coded("graph-not-exists", "Graph directory identity changed")
					return
				}
			}
			if operation["moved"] == true {
				dDev, dIno, dOK, dErr := directoryIdentity(destPath)
				if dErr != nil {
					err = dErr
					return
				}
				opSourceDev, opSourceIno := uint64Of(operation["source.dev"]), uint64Of(operation["source.ino"])
				if sOK || !dOK || opSourceDev != dDev || opSourceIno != dIno {
					err = coded("graph-not-exists", "Graph directory identity changed")
					return
				}
			} else if sOK {
				existed = true
				operation["existed"] = true
				if destPath == "" {
					parent := filepath.Join(ctx.GraphsDir, "Unlinked graphs")
					if err := os.MkdirAll(parent, 0o755); err != nil {
						err = coded("server-stop-failed", "%v", err)
						return
					}
					name := EncodeGraphName(repo)
					dest := filepath.Join(parent, name)
					for suffix := 1; fileExists(dest); suffix++ {
						dest = filepath.Join(parent, fmt.Sprintf("%s-%d", name, suffix))
					}
					operation["source"] = map[string]any{"dev": sDev, "ino": sIno}
					operation["destination"] = dest
					destPath = dest
					if err := writeJSON(ctx.StateFile, current); err != nil {
						return
					}
				}
				if renameErr := os.Rename(ctx.GraphDir, destPath); renameErr != nil {
					err = coded("server-stop-failed", "%v", renameErr)
					return
				}
				operation["moved"] = true
			}
		}()
		if err != nil {
			current["phase"] = "deletion-failed"
			current["error"] = err.Error()
			_ = writeJSON(ctx.StateFile, current)
			return nil, err
		}
		if err := writeJSON(ctx.StateFile, current); err != nil {
			return nil, err
		}
		current["phase"] = "deleted"
		delete(current, "error")
		if err := writeJSON(ctx.StateFile, current); err != nil {
			return nil, err
		}
		return map[string]any{"existed": existed, "destination": destOrNil(operation)}, nil
	})
	if err2 != nil {
		return false, "", err2
	}
	out := result.(map[string]any)
	return out["existed"] == true, stringOf(out["destination"]), nil
}

func destOrNil(operation map[string]any) any {
	if d := stringOf(operation["destination"]); d != "" {
		return d
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func uint64Of(v any) uint64 {
	switch t := v.(type) {
	case float64:
		return uint64(t)
	case int64:
		return uint64(t)
	case int:
		return uint64(t)
	case uint64:
		return t
	default:
		return 0
	}
}

func devOf(info os.FileInfo) uint64  { return uint64(sysDev(info)) }
func inoOf(info os.FileInfo) uint64  { return uint64(sysIno(info)) }

// StartOptions carries StartGraph inputs.
type StartOptions struct {
	Storage    *Storage
	Repo       string
	Script     string   // absolute path to db-worker-node.js
	NodeBin    string   // node executable (electron binary acts as node via ELECTRON_RUN_AS_NODE)
	Owner      string   // "electron" for the desktop host
	CreateEmpty bool
	Generation string   // expected generation ("" -> snapshot)
	ExtraArgs  []string // e.g. --embedding-endpoint ...
}

// StartGraph mirrors lifecycle startGraph: reuse a live worker or spawn a
// new one and poll until its /healthz reports ready. The returned map is
// the health payload plus the registration generation.
func StartGraph(opts StartOptions) (map[string]any, error) {
	ctx, err := Context(opts.Storage, opts.Repo)
	if err != nil {
		return nil, err
	}
	observed, err := Snapshot(opts.Storage, opts.Repo)
	if err != nil {
		return nil, err
	}
	observedGeneration := ""
	if observed != nil {
		observedGeneration = stringOf(observed["generation"])
	}
	owner := opts.Owner
	if owner == "" {
		owner = "cli"
	}
	var created map[string]any
	recordAny, err := withLease(ctx, "start", func(current map[string]any) (any, error) {
		generation := opts.Generation
		if generation == "" {
			generation = observedGeneration
		}
		if err := requireAvailable(ctx, current, generation); err != nil {
			return nil, err
		}
		if err := legacyCheck(ctx, current); err != nil {
			return nil, err
		}
		targets, err := discover(ctx, current)
		if err != nil {
			return nil, err
		}
		available := ownershipAvailable(ctx)
		live := []*target{}
		for _, t := range targets {
			if pidExists(t.PID) && (!available || isPending(t)) {
				live = append(live, t)
			}
		}
		if !available && len(live) == 0 {
			return nil, coded("repo-locked", "Graph ownership is locked without a registered worker")
		}
		if len(live) > 1 {
			return nil, coded("server-start-failed", "Multiple live graph workers")
		}
		if len(live) == 1 {
			t := live[0]
			return map[string]any{
				"ticket": t.Ticket, "generation": t.Generation, "pid": t.PID,
				"owner": t.Owner, "root": t.Root, "graphsDir": t.GraphsDir,
				"lifecycleDir": t.LifecycleDir, "repo": t.Repo,
				"ownership-protocol": t.OwnershipProtocol, "port": t.Port,
			}, nil
		}
		for _, t := range targets {
			if err := terminate(ctx, t, false); err != nil {
				return nil, err
			}
		}
		if err := cleanup(ctx, current, targets); err != nil {
			return nil, err
		}
		ticket := id()
		args := []string{
			opts.Script,
			"--repo", "logseq_db_" + ctx.Repo,
			"--root-dir", ctx.Root,
			"--graphs-dir", ctx.GraphsDir,
			"--lifecycle-dir", ctx.LifecycleDir,
			"--owner-source", owner,
			"--admission-ticket", ticket,
			"--graph-generation", stringOf(current["generation"]),
		}
		if opts.CreateEmpty {
			args = append(args, "--create-empty-db")
		}
		args = append(args, opts.ExtraArgs...)
		env := os.Environ()
		env = append(env, "ELECTRON_RUN_AS_NODE=1")
		if owner == "electron" {
			hasEmbedding := false
			for _, a := range opts.ExtraArgs {
				if a == "--embedding-endpoint" {
					hasEmbedding = true
					break
				}
			}
			if !hasEmbedding {
				filtered := env[:0]
				for _, kv := range env {
					if !strings.HasPrefix(kv, "LOGSEQ_EMBEDDINGS_URL=") {
						filtered = append(filtered, kv)
					}
				}
				env = filtered
			}
		}
		cmd := exec.Command(opts.NodeBin, args...)
		cmd.Env = env
		cmd.Stdout = nil
		cmd.Stderr = nil
		if owner != "electron" {
			cmd.SysProcAttr = detachedProcAttr()
		}
		if err := cmd.Start(); err != nil {
			return nil, coded("server-start-failed", "Worker failed to spawn: %v", err)
		}
		pid := cmd.Process.Pid
		if !pidExists(pid) {
			return nil, coded("server-start-failed", "Worker failed to spawn")
		}
		if owner != "electron" {
			releaseChild(cmd.Process)
		}
		trackChild(ticket, cmd.Process)
		go func(t string) {
			_ = cmd.Wait()
			untrackChild(t)
		}(ticket)
		spawned := runtimeRecord(ctx, ticket, stringOf(current["generation"]), pid, owner)
		spawned["parent"] = os.Getpid()
		spawned["expires"] = time.Now().UnixMilli() + 30000
		current["workers"] = []any{spawned}
		if err := writeJSON(ctx.StateFile, current); err != nil {
			_ = signalProcess(pid, syscall.SIGKILL)
			if !waitExit(pid, 2000) {
				return nil, fail("Unregistered worker did not exit")
			}
			return nil, err
		}
		created = spawned
		return spawned, nil
	})
	if err != nil {
		if created != nil {
			if cancelErr := cancelStartup(ctx, created); cancelErr != nil {
				return nil, coded("server-start-failed", "%s; startup cleanup failed: %s", err.Error(), cancelErr.Error())
			}
		}
		return nil, err
	}
	record := recordAny.(map[string]any)
	t := &target{
		Ticket: stringOf(record["ticket"]), Generation: stringOf(record["generation"]),
		PID: intOf(record["pid"]), Owner: stringOf(record["owner"]),
		Root: stringOf(record["root"]), GraphsDir: stringOf(record["graphsDir"]),
		LifecycleDir: stringOf(record["lifecycleDir"]), Repo: stringOf(record["repo"]),
		OwnershipProtocol: stringOf(record["ownership-protocol"]),
		Parent:            intOf(record["parent"]), Expires: int64Of(record["expires"]),
		Port: intOf(record["port"]),
	}
	// readiness poll (after lease release), 30s budget
	budget := 30000
	last := time.Now()
	for {
		if err := checkAdmission(ctx, record); err != nil {
			return nil, err
		}
		if !pidExists(t.PID) {
			return nil, coded("server-start-failed", "Worker exited before becoming ready")
		}
		runtime, err := readRuntime(ctx, record)
		if err == nil && runtime != nil {
			if port := intOf(runtime["port"]); port != 0 {
				t.Port = port
			}
		}
		if t.Port != 0 {
			value, err := health(ctx, t, t.Port)
			if err == nil && stringOf(value["status"]) == "ready" {
				value["generation"] = t.Generation
				return value, nil
			}
		}
		if budget <= 0 {
			return nil, coded("server-start-failed", "Worker failed to become ready")
		}
		time.Sleep(50 * time.Millisecond)
		now := time.Now()
		budget -= minOf(int(now.Sub(last).Milliseconds()), readyPollMaxElapsed)
		last = now
	}
}

func minOf(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// cancelStartup mirrors lifecycle cancelStartup.
func cancelStartup(ctx *GraphCtx, record map[string]any) error {
	_, err := withLease(ctx, "cancel-start", func(current map[string]any) (any, error) {
		if stringOf(current["generation"]) != stringOf(record["generation"]) || !registered(current, record) {
			if pidExists(intOf(record["pid"])) {
				return nil, fail("Startup registration changed while its worker remains alive")
			}
			return nil, nil
		}
		targets, err := discover(ctx, current)
		if err != nil {
			return nil, err
		}
		var match *target
		for _, t := range targets {
			if t.Ticket == stringOf(record["ticket"]) {
				match = t
				break
			}
		}
		if match != nil {
			if err := terminate(ctx, match, false); err != nil {
				return nil, err
			}
			if err := cleanup(ctx, current, []*target{match}); err != nil {
				return nil, err
			}
		}
		current["workers"] = filterWorkers(current, stringOf(record["ticket"]))
		return nil, writeJSON(ctx.StateFile, current)
	})
	return err
}

// StopOutdatedWorkers mirrors lifecycle stopOutdatedWorkers: at startup,
// stop workers whose daemon revision differs from the host's build.
func StopOutdatedWorkers(storage *Storage, revision string, repo string) error {
	if revision == "" {
		// Milestone-1 deviation: without a build revision we cannot compare
		// daemon builds, so no outdated-worker sweep runs.
		return nil
	}
	var names []string
	if repo != "" {
		names = []string{repo}
	} else {
		dirEntries, err := os.ReadDir(storage.GraphsDir)
		if err != nil {
			return err
		}
		for _, de := range dirEntries {
			if !de.IsDir() {
				continue
			}
			if name := canonicalGraphDirectory(de.Name()); name != "" {
				names = append(names, name)
			}
		}
	}
	var errs []error
	for _, name := range names {
		ctx, err := Context(storage, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := withLease(ctx, "upgrade", func(current map[string]any) (any, error) {
			if err := legacyCheck(ctx, current); err != nil {
				return nil, err
			}
			targets, err := discover(ctx, current)
			if err != nil {
				return nil, err
			}
			available := ownershipAvailable(ctx)
			var retired []*target
			for _, t := range targets {
				if available && !isPending(t) {
					if err := terminate(ctx, t, false); err != nil {
						return nil, err
					}
					if err := cleanup(ctx, current, []*target{t}); err != nil {
						return nil, err
					}
					current["workers"] = filterWorkers(current, t.Ticket)
					_ = writeJSON(ctx.StateFile, current)
					retired = append(retired, t)
					continue
				}
				if !pidExists(t.PID) {
					continue
				}
				if t.Port == 0 {
					return nil, coded("server-start-failed", "Worker endpoint is not published; retry after initialization")
				}
				value, err := health(ctx, t, t.Port)
				if err != nil {
					if !ignorableDiscoveryError(err) {
						return nil, err
					}
					if !waitExit(t.PID, 5000) {
						return nil, err
					}
					if err := cleanup(ctx, current, []*target{t}); err != nil {
						return nil, err
					}
					current["workers"] = filterWorkers(current, t.Ticket)
					_ = writeJSON(ctx.StateFile, current)
					retired = append(retired, t)
					continue
				}
				if stringOf(value["revision"]) == "" {
					return nil, fail("Worker revision is missing")
				}
				if stringOf(value["revision"]) == revision {
					continue
				}
				if err := terminate(ctx, t, false); err != nil {
					return nil, err
				}
				if err := cleanup(ctx, current, []*target{t}); err != nil {
					return nil, err
				}
				current["workers"] = filterWorkers(current, t.Ticket)
				_ = writeJSON(ctx.StateFile, current)
				retired = append(retired, t)
			}
			return retired, nil
		}); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return fail("Outdated worker cleanup failed: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// canonicalGraphDirectory mirrors lifecycle canonicalGraphDirectory.
func canonicalGraphDirectory(name string) string {
	if name == "" {
		return ""
	}
	decoded := lqgraph.DecodeGraphName(name)
	if decoded == "" {
		return ""
	}
	trimmed := strings.TrimSpace(decoded)
	if trimmed == "" || trimmed == "Unlinked graphs" || trimmed == "backup" ||
		strings.HasPrefix(trimmed, "logseq_db_") || strings.HasPrefix(trimmed, "logseq_local_") {
		return ""
	}
	enc, _ := lqgraph.EncodeGraphName(trimmed)
	if enc != name {
		return ""
	}
	return decoded
}

// observe polls state.json every 250ms and calls onChange when the graph
// generation/phase diverges from the watched generation (JS observe()).
func Observe(storage *Storage, repo, generation string, onChange func(current map[string]any)) (stop func()) {
	ctx, err := Context(storage, repo)
	if err != nil {
		onChange(map[string]any{"phase": "observation-failed", "error": err.Error()})
		return func() {}
	}
	closed := false
	previous := ""
	inspect := func() {
		if closed {
			return
		}
		current, _ := readJSON(ctx.StateFile)
		if current == nil || stringOf(current["generation"]) != generation || stringOf(current["phase"]) != "available" {
			key := stringOf(current["generation"]) + "|" + stringOf(current["phase"])
			if key != previous {
				previous = key
				onChange(current)
			}
			if stringOf(current["phase"]) != "deleting" {
				closed = true
			}
		}
	}
	inspect()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				inspect()
			}
		}
	}()
	return func() {
		if !closed {
			closed = true
		}
		close(done)
	}
}

var errAggregateSeparator = "; "

func aggregateError(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	return errors.New(strings.Join(msgs(errs), errAggregateSeparator))
}

func msgs(errs []error) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out
}
