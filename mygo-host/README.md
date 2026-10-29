# mygo-host — Go desktop host for Logseq (milestone 1)

A desktop host for the Logseq DB-graph renderer built on the
[mygo](https://github.com/egoist/mygo) framework, replacing the Electron
main process for milestone 1.

Status: **smoke-verified end to end** — renderer boots and mounts under the
Go host, graph-picker and graph views render, the `logseq-calendar` plugin
iframe loads, the Node db-worker daemon is spawned and passes the
admission/healthz identity check, and `db-worker-runtime` completes in ~1s.

## What it implements

| Renderer contract surface | Status | Notes |
|---|---|---|
| `lsp://` + `assets://` protocol routing | done | static dir, `~/.logseq/plugins`, `/external/<enc>/<rel>`, graph assets |
| `window.apis` preload shim (main IPC) | done | all `handler.cljs` commands dispatched over mygo bindings |
| Aux channels | done | call-application, call-main-win, toggle-max-or-min-active-win, set-quit-dirty-state, open-external/path |
| Sync roundtrip channel `:electron.server/sync!<id>` | accepted | reply channel only; daemon drives the request side |
| Renderer→main transit JSON payloads | done | decoded; plain JSON returned (renderer keywordizes) |
| DB-worker daemon (db-worker-node.js) lifecycle | done | startGraph/stopGraph/createGraph/deleteGraph/observe ported from `deps/graph-lifecycle` |
| Server-list discovery (`<root>/server-list`) | done | stale-pid cleanup, healthz identity check |
| `db-export` / `db-export-as` / `db-get` | done | via daemon `thread-api/backup-db-sqlite` + raw sqlite read |
| Graph registry (`~/.logseq/graphs.edn`) | done | upsert-entry / resolve-target |
| Dialogs / keychain / clipboard / shell | done | mygo Dialog/Secrets/Shell; clipboard via osascript/pbpaste (macOS) |
| http request commands + abort | done | returnType json/text/base64/arraybuffer |
| Proxy `setProxy` | partial | stored + applied to host HTTP clients; window session proxy not applied |
| Updater (`check-for-updates` etc.) | stub | replies `update-not-available` (mygo updater = milestone 2) |
| Semantic search / embeddings | stub | endpoint args pass-through only |
| db-sync local server (`server/*`) | stub | nil responses |
| Multi-window graph redirect (`redirectWhenExists`) | partial | window open/focus works; full redirect flow is milestone 2 |
| `runCli` | done | allowlist git/pandoc/ag/grep/alda |
| Marketplace plugin install/update/uninstall | stub | milestone 2 |
| x-callback-url / handbook deeplinks | done | `logseq://graph`, `new-window`, `quickCapture`, `invokeCommand`, `handbook` |
| Single instance + second-instance argv | done | |

## Build & run

```
cd logseq && pnpm gulp        # populate static/ (index.html, renderer js, db-worker-node.js)
cd logseq && clojure -M:cljs compile app db-worker db-worker-node
                              # REQUIRED after gulp: gulp:build's clean step deletes
                              # shadow-cljs outputs (static/js/main.js, static/db-worker-node.js)
cd mygo-host
CGO_ENABLED=0 go build -o mygo-host .
LOGSEQ_RESOURCES=../static ./mygo-host
```

The renderer must be built **from this branch** — it carries the platform
patches (`src/main/frontend/util.cljc` mygo detection,
`src/main/electron/ipc.cljs` plain-JSON IPC branch). A renderer built from
`master` has no `window.mygo` code path and falls back to memory-fs.

`mygo.json` next to `main.go` declares the app (name, identifier,
`urlSchemes: ["logseq"]`). Note: on macOS, deep-link (`logseq://`) scheme
registration requires the scheme inside a bundle's `Info.plist`
(`CFBundleURLTypes`) — a bare binary logs a registration warning and
continues without deep links. Producing the `.app` via `mygo build` is
milestone 2.

Requirements: Go 1.27+ (toolchain auto), the repo's `static/` build.
`go build ./... && go vet ./...` are clean; no third-party Go deps —
`github.com/egoist/mygo` only (replaced to the local checkout in go.mod).

## Debugging

- `LOGSEQ_DEBUG=1` attaches the debug probe: `OnDidFailLoad` logging, delayed
  page evals, and a `w.CapturePage()` written to `/tmp/mygo-capture.png` ~8s
  after launch. The renderer also collects boot errors into
  `window.__lqErrors` (installed by the preload before any app script).
- Diagnosing paint issues from another Space: `screencapture -l<WID>` and
  full-screen crops return white/wallpaper for windows on an inactive
  Space. Trust the in-process `CapturePage`, not screen captures.
- `LOGSEQ_WIN_CHROME=1` opts into `HiddenInset` titlebar chrome; on mygo
  v0.7.1 those options render blank on macOS, so they are off by default
  (bisected: either flag alone blanks the window; plain windows paint).
- The window is brought forward with `mygo.App.Focus()` after creation —
  plain executables do not activate on their own and a non-key window may
  never paint WKWebView content.
- Per-command IPC logging (`[ipc] -> cmd ok/ERROR`) goes to stdout.

## Environment

- `LOGSEQ_RESOURCES` — static dir (default: `<exe-dir>/static`, then `./static`)
- `LOGSEQ_DOT_DIR` — default `~/.logseq`
- `LOGSEQ_GRAPHS_DIR` — graphs root (shared with CLI/renderer defaults)
- `LOGSEQ_DB_WORKER_NODE_BIN` — node binary used to run the daemon
  (Electron uses its own binary with `ELECTRON_RUN_AS_NODE=1`)
- `LOGSEQ_ENTRY_URL` — default `lsp://logseq.com/index.html`
- `LOGSEQ_EXPECTED_REVISION` — expected daemon build revision; auto-extracted
  from `db-worker-node.js` when absent (dev builds: check skipped)

## Contract deviations (milestone 1)

1. **Lease + graph ownership use `flock`**, not SQLite `BEGIN IMMEDIATE`.
   The Node lifecycle writes `lease.sqlite` (graph-lifecycle dir) and
   `lock.sqlite` (`~/Library/Application Support/Logseq/runtime-locks/<sha>/`);
   the Go host uses sibling files `lease.lock` / `lock.lock` so it never
   touches the real SQLite files (no corruption risk). Consequence: Go-host
   and CLI lifecycle operations do not cross-serialize; each engine
   serializes its own processes. The daemon itself still self-registers via
   the real SQLite protocol, so mixed fleets work except for simultaneous
   lifecycle operations from both engines on the same graph.
2. **Daemon runtime uses `node` from PATH** (`LOGSEQ_DB_WORKER_NODE_BIN`)
   instead of the Electron binary with `ELECTRON_RUN_AS_NODE=1`.
3. **Transit JSON**: Electron main spoke transit both ways; the Go host
   decodes incoming transit and replies with plain JSON (renderer-side
   reader patched to accept both).
4. **`writeFileBytes` / clipboard reads are async** in the preload shim
   (Electron's were synchronous); renderer callers awaited them already.
5. **`getFilePath(File)`** returns `file.path` when the browser exposes it,
   else null (no webUtils equivalent).
6. **Windows-specific**: directory identity (dev/ino) is best-effort; custom
   scheme URLs are rewritten to `http://<scheme>.localhost/` so the
   `lsp://logseq.com|logseq.io` host routing falls back to path prefixes.
   SIGTERM stop stage degrades to TerminateProcess.
7. **Legacy retirement**: legacy artifacts (`db-worker.lock`, pre-sqlite-v1
   worker records) fail fast with "Legacy ownership unresolved" instead of
   being shut down and retired.
8. **`userAppCfgs`** is a JSON store at `<user-data>/configs.json`
   (Electron stored EDN `configs.edn`; the value is renderer-opaque).
9. **Keychain service** is `Logseq E2EE` via mygo Secrets (matches keytar
   `service = app name + " E2EE"`).
10. **Updater is a stub** (no-update) — mygo updater integration lands in
    milestone 2.

## Layout

```
mygo-host/
├── main.go
└── internal/
    ├── host/       app wiring, IPC dispatch, protocols, preload, events
    ├── manager/    per-graph worker registry (db_worker.cljs port)
    ├── daemon/     cli-server ensure/stop/discover (cli/server.cljs port)
    ├── lifecycle/  graph-lifecycle port (lease/ownership/start/stop/delete)
    ├── transit/    transit-json decoder subset
    ├── edn/        EDN subset parser/printer (graphs.edn, metadata)
    └── lqgraph/    graph-name/registry/path helpers (common/* ports)
```
