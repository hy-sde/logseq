# Shared by build-app.sh and build-dist.sh: stage the renderer and the
# embedding sidecar into mygo-host/resources/ so `mygo build` ships them
# inside every package (macOS Contents/Resources/static, the Windows/Linux
# payload directories). Callers define host_dir and must have set -euo
# pipefail. Requires ../static to exist (build the renderer first).

repo_static="$host_dir/../static"
if [ ! -d "$repo_static" ]; then
  echo "[mygo-build] missing $repo_static — build the renderer first:" >&2
  echo "  cd $(cd "$host_dir/.." && pwd) && pnpm gulp && clojure -M:cljs compile app db-worker db-worker-node" >&2
  exit 1
fi

# The node sidecar must be a self-contained release build: a dev-mode
# db-worker-node.js lazily loads <repo>/.shadow-cljs/.../cljs-runtime from
# disk, which does not exist inside a package (daemon exits with ENOENT).
if grep -q "SHADOW_IMPORT_PATH" "$repo_static/db-worker-node.js"; then
  echo "[mygo-build] static/db-worker-node.js is a dev build (lazy cljs-runtime loads); building release sidecar"
  (cd "$host_dir/.." && clojure -M:cljs release db-worker-node)
fi
if grep -q "SHADOW_IMPORT_PATH" "$repo_static/db-worker-node.js"; then
  echo "[mygo-build] db-worker-node.js still carries SHADOW_IMPORT_PATH after release build — aborting" >&2
  exit 1
fi

# Ship the renderer inside the package: mygo copies the resources/ directory
# into every target's payload.
rm -rf "$host_dir/resources/static"
mkdir -p "$host_dir/resources"
cp -R "$repo_static" "$host_dir/resources/static"

# Stage the npm closure the sidecar require()s at runtime (the repo's
# node_modules is not visible inside a package).
node "$host_dir/scripts/stage-node-modules.mjs" \
  "$repo_static/db-worker-node.js" "$host_dir/resources"

# Ship the python embedding sidecar next to static; the host runs its
# embedding_server.py through a user-data venv at runtime.
repo_sidecar="$host_dir/../sidecar"
rm -rf "$host_dir/resources/sidecar"
if [ -d "$repo_sidecar" ]; then
  cp -R "$repo_sidecar" "$host_dir/resources/sidecar"
else
  echo "[mygo-build] note: $repo_sidecar missing — embedding sidecar will be unavailable"
fi
