#!/usr/bin/env bash
# Build the macOS Logseq.app bundle with `mygo build` (deep-link logseq://
# scheme registration comes from mygo.json urlSchemes -> CFBundleURLTypes
# in the bundle's Info.plist — a bare binary cannot register the scheme).
#
# The built renderer (../static) is copied into mygo resources/ so the
# bundle ships it at Contents/Resources/static; the host resolves that
# layout in defaultResourcesDir. Re-run this script after rebuilding the
# renderer.
#
# Usage: scripts/build-app.sh [extra mygo build flags...]
#   e.g. scripts/build-app.sh -debug     # keep the web inspector
#
# Env:
#   MYGO_BIN  path to the mygo CLI (default: PATH, else built from the
#             pinned mygo checkout via the go.mod replace directive)
set -euo pipefail

host_dir="$(cd "$(dirname "$0")/.." && pwd)"
repo_static="$host_dir/../static"

if [ -z "${MYGO_BIN:-}" ]; then
  if command -v mygo >/dev/null 2>&1; then
    MYGO_BIN="$(command -v mygo)"
  else
    echo "[build-app] mygo CLI not on PATH; building it from the pinned mygo checkout"
    mkdir -p "$host_dir/.tools"
    (cd "$host_dir" && go build -o .tools/mygo github.com/egoist/mygo/cmd/mygo)
    MYGO_BIN="$host_dir/.tools/mygo"
  fi
fi

if [ ! -d "$repo_static" ]; then
  echo "[build-app] missing $repo_static — build the renderer first:" >&2
  echo "  cd $(cd "$host_dir/.." && pwd) && pnpm gulp && clojure -M:cljs compile app db-worker db-worker-node" >&2
  exit 1
fi

# Ship the renderer inside the bundle: mygo copies the resources/ directory
# into Contents/Resources.
rm -rf "$host_dir/resources/static"
mkdir -p "$host_dir/resources"
cp -R "$repo_static" "$host_dir/resources/static"

echo "[build-app] building with $MYGO_BIN"
(cd "$host_dir" && "$MYGO_BIN" build -skip-dmg -skip-notarize -sign - "$@")

app="$(ls -d "$host_dir"/dist/*/Logseq.app "$host_dir"/dist/Logseq.app 2>/dev/null | head -1 || true)"
if [ -z "$app" ]; then
  echo "[build-app] no Logseq.app under $host_dir/dist — see mygo build output above" >&2
  exit 1
fi
echo "[build-app] built $app"
echo "[build-app] URL schemes:"
/usr/libexec/PlistBuddy -c "Print :CFBundleURLTypes" "$app/Contents/Info.plist"
