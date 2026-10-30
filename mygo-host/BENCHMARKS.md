# Logseq-mygo vs stock Logseq — benchmark results

Measured on the maintainer's machine (macOS 26.6.2, Apple Silicon, 2026-10-30), comparing
**stock Logseq** (`/Applications/Logseq.app`, Electron 37.x) against **Logseq-mygo**
(`/Users/hui/Applications/Logseq-mygo.app`, built from this branch at `c3d87a29d1`).

Methodology mirrors the mygo author's published benchmark families
([mygo.egoist.dev/benchmarks](https://mygo.egoist.dev/benchmarks)): the families that apply to a
webview host are **IPC round-trip, window open, bundle size, idle memory**. The native-toolkit
families (GPU frames, text layout, native UI diffing) only measure mygo's `ui/` toolkit and do not
apply to either webview app.

Reference points from the author's suite (darwin, webview-relevant only): webview IPC e2e
`PageCall` **212 µs**, `WindowOpen` **118 ms**, hello binary **9.6 MB**, hello app RSS **26 MB**.

## Results

3 runs each; each run uses an isolated `HOME`, fresh dot-dir/graphs-dir, stock not running.

| Metric | Stock Electron | Logseq-mygo | Δ |
|---|---|---|---|
| First window onscreen (median) | 1,558 ms (1,300 / 1,558; true-cold 3,871) | **59 ms** (49 / 59 / 85) | **~26× faster** |
| UI usable (graph view mounted) | window at 3.9 s cold, then settle | window instant; graph mounted at +3 s, daemon ready +4 s | comparable; ours visible immediately |
| Idle process-tree RSS | 849–871 MB across 4 processes | **557–630 MB** across 3 | **−30 %** |
| — main app process | 250–253 MB | **120–132 MB** | **−52 %** |
| — without embedding sidecar | n/a | ~310–350 MB (app 120 + node daemon 185–220) | **−60 %** |
| IPC round-trip (200 × `getAppBaseInfo`) | **95 µs/call** | 195 µs/call | ~2× slower |
| Bundle size | 508 MB (268 MB Electron frameworks) | **349 MB** (11.5 MB Go binary + 253 MB web assets) | **−31 %** |
| Main binary | 52 KB launcher + 268 MB frameworks | **11.5 MB** static Go binary | — |

Raw per-run data:

```
stock1 pid=10985 window_ms=3871 rss=856272 KB  (main 250MB + renderer 428MB + helpers 58+98MB)
stock2 pid=11064 window_ms=1300 rss=849200 KB  (main 253MB + renderer 425MB + helpers 53+96MB)
stock3 pid=11079 window_ms=1558 rss=870736 KB  (main 253MB + renderer 442MB + helpers 56+97MB)
mygo1  pid=11024 window_ms=49   rss=557408 KB  (app 120MB + node daemon 218MB + python sidecar 205MB)
mygo2  pid=11111 window_ms=85   rss=572304 KB  (app 121MB + node daemon 184MB + python sidecar 252MB)
mygo3  pid=11130 window_ms=59   rss=629840 KB  (app 132MB + node daemon 182MB + python sidecar 300MB)
```

## Interpretation

**Wins**

- **Window open ~26× faster.** mygo creates a native window immediately; Electron pays full
  framework boot before its window exists. The mygo author's own `WindowOpen` reference is 118 ms
  for a tiny app; our 59 ms median is for the full Logseq renderer.
- **Memory −30 % overall, −52 % for the app process itself.** Notably our tree was doing *more*
  work (graph open + db-worker daemon + embedding sidecar) while stock's fresh-run tree never
  opened a DB graph.
- **Bundle −31 %**, and the host is a single static 11.5 MB Go binary with no framework —
  distribution, update payloads, and CI all get simpler.

**Honest losses**

- **IPC ~2× slower** (95 µs → 195 µs). Electron's `ipcRenderer.invoke` uses an optimized
  main-process channel; our bridge crosses the WKWebView postMessage boundary. Both are sub-millisecond;
  our 195 µs sits right at the author's own `PageCall` reference (212 µs), so this is the webview
  round-trip cost, not a defect in our bridge.
- **Embedding sidecar auto-spawns** on fresh dots (205→300 MB across runs) — stock keeps it gated.
  Known follow-up: gate it behind the semantic-search toggle.

**Caveats**

- Stock's tree shows no db-worker daemon: its first-run flow did not open a DB graph in the
  18 s settle window. Our numbers include a fully-open graph.
- "Window onscreen" for our app measures window *existence*; content paints asynchronously
  (graph picker usable ≈ 3 s, consistent with Electron's time-to-interactive after its 3.9 s
  window). First-window latency is where the user perceives the difference.

## Reproducing

Prereqs: built renderer (`pnpm gulp && clojure -M:cljs compile app db-worker db-worker-node`),
installed stock Logseq, our app bundle, `node >= 22`, `python3`, `swiftc`.

```sh
cd mygo-host/benchmarks
# one-shot: spawn, wait for first window, sleep 18s, process-tree RSS, kill
env HOME=/tmp/lqbench ./run-one.sh stock1 /Applications/Logseq.app/Contents/MacOS/Logseq
env LOGSEQ_DOT_DIR=/tmp/lqbench/mygo-dot LOGSEQ_GRAPHS_DIR=/tmp/lqbench/mygo-graphs \
  ./run-one.sh mygo1 /Users/hui/Applications/Logseq-mygo.app/Contents/MacOS/Logseq
```

- `winpoll.swift` → `winpoll` — polls CGWindowList (50 ms) for the first onscreen normal-layer
  window of the spawned pid; prints elapsed ms.
- `tree_rss.py <pid>` — sums RSS of the process tree (`ps` walk), lists >20 MB members.
- `cdp-ipc.mjs <port> <n>` — stock-side IPC micro-bench: `node cdp-ipc.mjs 9222 200` against a
  stock instance launched with `--remote-debugging-port=9222`, timing N ×
  `window.apis.doAction(["getAppBaseInfo"])`.
- mygo-side IPC micro-bench: build with `LOGSEQ_IPCBENCH=1` support (already in
  `internal/host/debug_probe.go`) and run with `LOGSEQ_IPCBENCH=1`; after 25 s it logs
  `[bench] ipc: 200 getAppBaseInfo round-trips took N ms`.

All harness files are read-only instrumentation; nothing is written outside the isolated dirs.
