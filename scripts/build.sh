#!/usr/bin/env bash
# Cross-compile rsb's three binaries for the supported target platforms.
#
# Targets (主流三平台):
#   linux/amd64   — 绝大多数云服务器
#   linux/arm64   — ARM 服务器 / 树莓派 / Graviton
#   darwin/arm64  — Apple Silicon Mac
#
# Output: dist/<os>-<arch>/<binary>
# rsb and rsb-daemon are CLIENT-side (run where the agent runs).
# rsb-agent is REMOTE-side (must match the server's os/arch, not the client's).
#
# Usage:
#   ./build.sh              # build all targets
#   ./build.sh linux-arm64  # build one target
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-0.5.1}"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Common flags; each binary gets its own -X for its version variable name.
LDFLAGS_COMMON="-s -w"  # strip debug info
TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/arm64"
)
DIST="skill/bin"
BINARIES=("rsb" "rsb-agent" "rsb-daemon")

if [[ $# -gt 0 ]]; then
  TARGETS=("$@")
fi

rm -rf "$DIST"
mkdir -p "$DIST"

# ldflagsFor returns the ldflags for a given binary: inject the version into
# the right package variable. cmd/rsb uses main.version; cmd/rsb-agent uses
# main.agentVersion + main.buildTime; cmd/rsb-daemon has no version yet.
ldflagsFor() {
  local bin="$1"
  case "$bin" in
    rsb)       echo "-X main.version=${VERSION}" ;;
    rsb-agent) echo "-X main.agentVersion=${VERSION} -X main.buildTime=${BUILD_TIME}" ;;
    *)         echo "" ;;
  esac
}

rm -rf "$DIST"
mkdir -p "$DIST"

for target in "${TARGETS[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  outdir="$DIST/${os}-${arch}"
  mkdir -p "$outdir"

  echo "==> building $target"
  for bin in "${BINARIES[@]}"; do
    echo "    $bin"
    GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
      go build -trimpath -ldflags "$LDFLAGS_COMMON $(ldflagsFor "$bin")" \
      -o "$outdir/$bin" "./cmd/$bin"
  done
done

echo ""
echo "==> built binaries:"
find "$DIST" -type f | sort | while read -r f; do
  size=$(du -h "$f" | cut -f1)
  echo "    $f ($size)"
done

# Also produce sha256 for verification (agent/skill can check integrity).
echo ""
echo "==> checksums:"
if command -v shasum >/dev/null 2>&1; then
  (cd "$DIST" && find . -type f -exec shasum -a 256 {} \; | sort > SHA256SUMS)
  echo "    $DIST/SHA256SUMS"
fi
