#!/usr/bin/env bash
# Build distributable artifacts for every platform with `mygo build`.
#
# Artifacts land in mygo-host/dist/<os>-<arch>/ (mygo's layout):
#   darwin-<arch>/    Logseq.app, logseq-<version>-darwin-<arch>.dmg, install.sh
#   windows-<arch>/   Logseq.exe, logseq-setup-<version>-<arch>.exe
#                     (the NSIS installer is only built when makensis is
#                     installed; otherwise mygo skips it with a note)
#   linux-<arch>/     logseq, logseq.desktop, logseq-<version>-linux-<arch>.tar.gz,
#                     logseq_<version>-<arch>.deb
#
# Without credentials everything is ad-hoc signed or unsigned. Credentials:
#   macOS signing   LOGSEQ_SIGN_IDENTITY="Developer ID Application: … (TEAMID)"
#                   (passed to mygo build -sign)
#   macOS notarize  macos.notarize.keychainProfile in mygo.json
#                   (xcrun notarytool store-credentials) — runs only when a
#                   Developer ID identity signs and the profile is set
#   Windows signing windows.certificate (pfx) + MYGO_WINDOWS_CERTIFICATE_PASSWORD,
#                   or windows.signCommand, in mygo.json
# See mygo docs/distribution.md for the full reference.
#
# Usage: scripts/build-dist.sh [target...]   (default: all)
#   targets: darwin/arm64 darwin/amd64 windows/amd64 linux/amd64 linux/arm64
# Env:
#   LOGSEQ_SIGN_IDENTITY  macOS Developer ID identity (optional, see above)
#   MYGO_BIN              mygo CLI path (default: PATH, else built from the
#                         pinned mygo checkout via go.mod replace)
#   SKIP_VERIFY=1         skip the artifact structure verification
set -euo pipefail

host_dir="$(cd "$(dirname "$0")/.." && pwd)"
cd "$host_dir"

targets=("$@")
if [ ${#targets[@]} -eq 0 ]; then
  targets=(darwin/arm64 darwin/amd64 windows/amd64 linux/amd64 linux/arm64)
fi

if [ -z "${MYGO_BIN:-}" ]; then
  if command -v mygo >/dev/null 2>&1; then
    MYGO_BIN="$(command -v mygo)"
  else
    echo "[build-dist] mygo CLI not on PATH; building it from the pinned mygo checkout"
    mkdir -p "$host_dir/.tools"
    go build -o .tools/mygo github.com/egoist/mygo/cmd/mygo
    MYGO_BIN="$host_dir/.tools/mygo"
  fi
fi

# Stage renderer + sidecar into resources/ (shared with build-app.sh).
source "$host_dir/scripts/_resources.sh"

flags=(-sign - -skip-notarize)
if [ -n "${LOGSEQ_SIGN_IDENTITY:-}" ]; then
  echo "[build-dist] signing macOS builds with: $LOGSEQ_SIGN_IDENTITY"
  flags=(-sign "$LOGSEQ_SIGN_IDENTITY" -skip-notarize)
fi

platform_args=""
for t in "${targets[@]}"; do
  platform_args="$platform_args$t,"
done
platform_args="${platform_args%,}"

echo "[build-dist] building targets: $platform_args"
"$MYGO_BIN" build -platform "$platform_args" "${flags[@]}"

if [ "${SKIP_VERIFY:-}" = "1" ]; then
  echo "[build-dist] SKIP_VERIFY=1 — skipping artifact verification"
  exit 0
fi

echo "[build-dist] verifying artifacts"
fail=0
verify() {
  local path="$1" what="$2"
  if [ -e "$path" ]; then
    printf '  ok  %-58s %s\n' "$what" "$(du -h "$path" | cut -f1 | tr -d ' ')"
  else
    printf '  MISSING  %s (%s)\n' "$what" "$path"
    fail=1
  fi
}

for t in "${targets[@]}"; do
  os="${t%/*}"; arch="${t#*/}"
  dir="dist/$os-$arch"
  case "$os" in
    darwin)
      verify "$dir/Logseq.app" "macOS app bundle ($t)"
      dmg="$(ls "$dir"/logseq-*.dmg "$dir"/Logseq-*.dmg 2>/dev/null | head -1 || true)"
      if [ -n "$dmg" ]; then
        verify "$dmg" "macOS DMG ($t)"
        if hdiutil imageinfo "$dmg" >/dev/null 2>&1; then
          echo "  ok       DMG imageinfo ($t)"
        else
          echo "  FAILED   hdiutil imageinfo $dmg"; fail=1
        fi
        mnt="$(mktemp -d)"
        if hdiutil attach "$dmg" -readonly -nobrowse -mountpoint "$mnt" >/dev/null &&
          [ -e "$mnt"/Logseq.app ]; then
          echo "  ok       DMG attach + Logseq.app present ($t)"
          hdiutil detach "$mnt" >/dev/null
        else
          echo "  FAILED   DMG attach ($t)"; fail=1
        fi
        rm -rf "$mnt"
      else
        echo "  MISSING  macOS DMG ($t)"; fail=1
      fi
      ;;
    windows)
      verify "$dir/Logseq.exe" "Windows executable ($t)"
      setup="$(ls "$dir"/logseq-setup-*.exe "$dir"/Logseq-setup-*.exe 2>/dev/null | head -1 || true)"
      if [ -n "$setup" ]; then
        verify "$setup" "Windows NSIS installer ($t)"
      else
        echo "  note     no NSIS installer for $t (makensis not installed;" \
          "install it, e.g. brew install makensis, and rerun)"
      fi
      ;;
    linux)
      verify "$dir/logseq" "Linux executable ($t)"
      verify "$dir/logseq.desktop" "Linux desktop entry ($t)"
      verify "$(ls "$dir"/logseq-*.tar.gz 2>/dev/null | head -1)" "Linux tar.gz ($t)"
      deb="$(ls "$dir"/logseq_*.deb "$dir"/logseq-*.deb 2>/dev/null | head -1 || true)"
      if [ -n "$deb" ]; then
        verify "$deb" "Linux .deb ($t)"
        # dpkg-deb is absent on macOS; ar lists the deb members.
        members="$(ar t "$deb" 2>/dev/null | tr '\n' ' ')"
        case "$members" in
          *debian-binary*control.tar*data.tar*)
            echo "  ok       .deb members: $members" ;;
          *)
            echo "  FAILED   unexpected .deb members: $members"; fail=1 ;;
        esac
      else
        echo "  MISSING  Linux .deb ($t)"; fail=1
      fi
      ;;
  esac
done

if [ "$fail" -ne 0 ]; then
  echo "[build-dist] verification FAILED — see above" >&2
  exit 1
fi
echo "[build-dist] all artifacts verified under $host_dir/dist/"
