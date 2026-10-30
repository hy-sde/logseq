# mygo-host — Go desktop host for Logseq (milestone 2)

A desktop host for the Logseq DB-graph renderer built on the
[mygo](https://github.com/egoist/mygo) framework, replacing the Electron
main process; milestone 2 ports the remaining Electron main-process
surfaces (marketplace plugins, updater, app menu, quit flow, deep-link
app bundle).

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
| Updater (`check-for-updates` etc.) | done | GitHub latest-release check (`LOGSEQ_UPDATES_URL` override) vs `LOGSEQ_VERSION`/mygo.json; `updates-callback` events (`internal/host/updater.go`) |
| Semantic search / embeddings | done (macOS) | sidecar manager (`internal/host/embedding.go`): venv + `sidecar/embedding_server.py`, publishes `LOGSEQ_EMBEDDINGS_URL` for db-worker spawns |
| db-sync local server (`server/*`) | stub | nil responses |
| Multi-window graph redirect (`redirectWhenExists`) | done | `logseq://graph/<id>?page|block-id|file` redirects the window on that graph; `logseq://new-window/...` opens a renderer-driven second window and replays the redirect at `graphReady` |
| `runCli` | done | allowlist git/pandoc/ag/grep/alda |
| Deep links via bundled .app | done | `scripts/build-app.sh` → signed `Logseq.app` with `CFBundleURLTypes`; `OnOpenURL` dispatch verified |
| Marketplace plugin install/update/uninstall | done | GitHub release zip → `<dotdir>/plugins/<id>`; `lsp-updates` events (`internal/host/marketplace.go`, plugin.cljs port) |
| x-callback-url / handbook deeplinks | done | `logseq://graph`, `new-window`, `quickCapture`, `invokeCommand`, `handbook` |
| Single instance + second-instance argv | done | |
| App menu (`set-app-menu!` port) | done | app/file/edit/view/window/help roles + Always-on-Top checkbox + Cmd/Ctrl+= zoom (`internal/host/menu.go`) |
| Quit flow (before-quit / window-all-closed) | done | db-worker daemons stopped on both paths; confirm dialog when the renderer's quit-dirty flag is set (mygo Dialog, `PreventDefault` on cancel) |

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

`mygo.json` next to `main.go` declares the app (name, identifier, version,
`urlSchemes: ["logseq"]`). On macOS the `logseq://` scheme is only
registered from a bundle's `Info.plist` (`CFBundleURLTypes`) — use
`scripts/build-app.sh` to produce a signed `Logseq.app`:

```
cd mygo-host && scripts/build-app.sh        # add -debug for the web inspector
open dist/darwin-arm64/Logseq.app
```

The script copies the built renderer (`../static`) into the bundle
(`Contents/Resources/static`, resolved by `defaultResourcesDir`) and builds
the CLI from the pinned mygo checkout when `mygo` is not on `PATH`.
Deep-link smoke: `open -a <path-to-built-Logseq.app> "logseq://graph/<name>"`
(`open -b com.logseq.logseq` routes to an installed release Logseq with the
same bundle id — target the built bundle by path).

Requirements: Go 1.27+ (toolchain auto), the repo's `static/` build.
`go build ./... && go vet ./...` are clean; no third-party Go deps —
`github.com/egoist/mygo` only (replaced to the local checkout in go.mod).

## Packaging

`scripts/build-dist.sh` produces the distributable artifacts for all
platforms via `mygo build` cross-compilation (no credentials needed for
unsigned builds):

```
cd mygo-host && scripts/build-dist.sh                       # all five targets
scripts/build-dist.sh darwin/arm64 darwin/amd64             # subset
SKIP_VERIFY=1 scripts/build-dist.sh ...                     # skip structure checks
LOGSEQ_SIGN_IDENTITY="Developer ID Application: …" scripts/build-dist.sh darwin/arm64
```

Artifacts land in `dist/<os>-<arch>/` (gitignored):

| Target | Contents |
|---|---|
| `darwin/arm64`, `darwin/amd64` | `Logseq.app` + `logseq-<version>-<arch>.dmg` + `install.sh` |
| `windows/amd64` | `Logseq.exe` + NSIS `logseq-setup-<version>-<arch>.exe` + `install.sh` |
| `linux/amd64`, `linux/arm64` | `logseq` binary + `logseq.desktop` + `logseq-<version>-<os>-<arch>.tar.gz` + `logseq_<version>_<arch>.deb` + `install.sh` |

The script stages the renderer (`../static`) and the embedding sidecar into
`resources/` via `scripts/_resources.sh` (shared with `build-app.sh`), then
verifies each artifact's structure: DMG `hdiutil imageinfo` + attach/detach
of the contained `Logseq.app`, `.deb` `ar t` members
(`debian-binary/control.tar/data.tar`), installer/exe presence for windows.
On macOS hosts without `makensis` the Windows installer is skipped with a
note (`brew install makensis` and rerun — mygo only auto-installs NSIS on
Windows); the `.deb`/tar.gz/AppImage-less linux layout follows mygo's
packaging (no AppImage support).

Signing / notarization credentials (all optional — unsigned builds work
everywhere):

- macOS code signing: `LOGSEQ_SIGN_IDENTITY` (a `Developer ID Application:
  …` identity), passed to `mygo build -sign`. `-` means ad-hoc sign (the
  default here); an empty value would produce an unsigned build, so the
  script always passes an identity. Notarization is `-skip-notarize` here;
  for a notarized build call `mygo build` directly with
  `macos.notarize.keychainProfile` (a keychain profile created via
  `xcrun notarytool store-credentials`) in `mygo.json` and no
  `-skip-notarize` flag (see mygo docs/distribution.md).
- Windows signing: set `windows.certificate` (a .pfx) plus
  `MYGO_WINDOWS_CERTIFICATE_PASSWORD`, or `windows.signCommand`, in
  `mygo.json` before building (mygo docs/distribution.md).

## Updates (Ed25519-signed manifest feed)

The host's updater (`internal/host/updater.go`) accepts two feed modes:

- **GitHub-release mode** (default): `check-for-updates` reads the latest
  `logseq/logseq` GitHub release (`LOGSEQ_UPDATES_URL` overrides the
  endpoint).
- **Manifest mode** (`LOGSEQ_UPDATE_PUBLIC_KEY` set): `LOGSEQ_UPDATES_URL`
  points at a mygo `update-<target>.json` manifest
  (docs/updates.md schema). The manifest is Ed25519-verified before any
  update is announced; the archive itself carries mygo's signature over
  its SHA-256 digest and is verified by the updater at download time.
  Emitted `updates-callback` payloads are identical to GitHub mode.

Publishing a release:

```
mygo keygen -o ~/.config/mygo/update-keys     # mygo-update.key (SECRET) + .pub
scripts/build-dist.sh darwin/arm64            # or the full matrix
go build -o /tmp/gen-update-manifest ./cmd/gen-update-manifest
/tmp/gen-update-manifest \
  -payload dist/darwin-arm64 -target darwin-arm64 -version 0.0.2 \
  -base-url https://updates.example.com/logseq \
  -notes-file release-notes.md \
  -key-file ~/.config/mygo/update-keys/mygo-update.key \
  -out dist/publish
# → dist/publish/logseq-0.0.2-darwin-arm64.tar.gz + update-darwin-arm64.json
# round-trip check before publishing:
/tmp/gen-update-manifest -verify -dir dist/publish -target darwin-arm64 \
  -pub-file ~/.config/mygo/update-keys/mygo-update.pub
```

Then publish both files to a GitHub release (tag `v0.0.2` — upload the
manifests LAST: installed apps read `update-<target>.json` of the latest
release) or copy them to the HTTPS directory of `-base-url`. The
application-side feed needs `LOGSEQ_UPDATE_PUBLIC_KEY` = the base64
contents of `mygo-update.pub` and `LOGSEQ_UPDATES_URL` = the manifest URL.
Private keys are gitignored (`mygo-update.key`); only `.pub` values ever
enter a repo or CI secret (`MYGO_UPDATER_PRIVATE_KEY` for `mygo build`
delta generation). `install-updates` still cannot self-replace a running
binary — see milestone-2 deviation 5.

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
   instead of the Electron binary with `ELECTRON_RUN_AS_NODE=1`. GUI
   launches (Finder/`open` get launchd's minimal PATH) fall back to
   Homebrew, /usr/local, Volta and the newest nvm install when `node`
   is not on PATH.
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
 10. **Updater is implemented in milestone 2** (GitHub latest-release check
    via `check-for-updates`; see milestone-2 deviations 4–5).

## Contract deviations (milestone 2)

1. **Marketplace GitHub API probing** (plugin.cljs `valid-github-api!`):
   the `plugins.logseq.io/github/api` proxy is probed first (2s
   `/rate_limit`, 60s cache) and falls back to `api.github.com`, matching
   Electron. Requests honor the renderer's stored proxy options (http
   proxies; socks5 unsupported — stdlib limitation).
2. **Semver comparisons** (`semverCoerce`/`semverCompare` in
   `marketplace.go`) ignore prerelease suffixes; marketplace version gates
   are numeric `major.minor.patch` comparisons, matching `semver.coerce`
   for the values plugins ship. An update whose latest tag has no numeric
   core skips the no-new-version gate and installs, matching Electron's
   `when-let` over `(semver.coerce latest-version)`; a version-pinned
   install falls back to the latest release only on GitHub 404 (other
   API failures surface as errors).
   Proxy options use the renderer's shape (`type` + string `port` from
   `util/normalize-port-input`); an unparseable port applies no proxy
   rather than erroring the request.
3. **`download-asset-zip` error double-emit** is collapsed: Electron emits
   an extra raw-error `lsp-updates` payload before the structured
   `{status "error", payload {..., error-code}}` one; the host emits only
   the structured payload (the renderer consumes that one).
4. **Updater has no auto-download**: `check-for-updates` fetches the
   latest `logseq/logseq` GitHub release (`LOGSEQ_UPDATES_URL` overrides
   the endpoint; expected shape = GitHub release JSON) and pushes the
   `updates-callback` sequence `checking-for-update →
   update-available|update-not-available|error → completed`. There is no
   download progress / downloaded state; `update-available` payload
   carries `{version, name, releaseName, releaseNotes, releaseDate, url}`.
   The boot-time auto-check (Electron `init-auto-updater!`, prod only) is
   skipped — updates are checked from Settings.
5. **`install-updates` (installUpdatesAndQuitApp) cannot self-replace** a
   running Go binary: it logs the limitation and opens the GitHub release
   page in the browser instead of quitting + installing.
6. **Preload shim argument convention**: mygo maps JS call arguments
   positionally onto Go method params, so channel arguments are passed as
   one array (`call('LqChannels.Handle', channel, args)`); Electron passes
   spread arguments over `ipcRenderer.invoke`. Same wire payload, different
   JS-side packing (`internal/host/preload.go`).
7. **`redirectWhenExists` targeting** mirrors `url.cljs` with one
   deviation: Electron drops the `openNewWindowOfGraph` send when no
   window is focused (`send-to-renderer` with a nil window is a no-op);
   the host falls back to the first alive window so a deep link always
   opens a graph window. Redirect payloads send JSON `null` for absent
   `page`/`block-id`/`file` params (empty strings are truthy in CLJS).
   The pending-redirect slot is consumed by the first `graphReady`
   regardless of the reported graph, matching the Electron
   `:window/once-graph-ready` slot.
8. **Menu labels are plain English** (the Go host has no main-process
   i18n; Electron resolves `:electron/new-window` etc. from its dicts).
   Non-mac About shows a plain message dialog without the app icon asset.
9. **Menu close accelerators**: the mac File menu's "Close Window" has no
   accelerator (Electron disables Command+W via `:accelerator false`);
   the non-mac Window menu keeps mygo's default Close accelerator
   (Electron disables Control+W there).
10. **Quit confirm dialog fires whenever the dirty flag is set**: Electron
   only consults `*quit-dirty?` inside its update-restart close handler;
   the host shows the "Quit Logseq?" dialog on every quit whose
   `set-quit-dirty-state` flag is still true (the renderer clears it only
   for the update flow), so a normal quit asks for confirmation where
   Electron would not. Dialog errors fail open (quit proceeds).
 11. **Embedding sidecar runtime dir** lives at `<dot-dir>/embedding-server`
    (Electron uses its user-data dir) and the venv bootstrap mirrors
    `embedding_server.cljs` (reuse usable venv, recreate broken ones,
    `deps-v2.ok` stamp gates pip installs). The sidecar script ships at
    `Contents/Resources/sidecar` via `scripts/build-app.sh`. macOS-only,
    like the Electron implementation. Workers spawned before the sidecar
    is ready miss `LOGSEQ_EMBEDDINGS_URL` until their next restart (same
    race as Electron's async `setup!`); the sidecar is killed with its
    process group on quit (before-quit + window-all-closed).

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
