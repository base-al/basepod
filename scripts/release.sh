#!/usr/bin/env bash
# Build the release artifacts for a version: one tarball per supported
# platform plus checksums.txt, matching the layout every release since
# v0.1.0 has used (each tarball holds the `basepod` binary at its root).
#
# This exists because the release had been cut by hand seven times, and
# the steps that are easy to forget are the ones that matter: building
# the dashboard *before* the binary (the binary embeds web/dist via
# go:embed, so a stale dist ships silently), and stamping the version
# through -ldflags (an unstamped binary reports "dev" in the UI and in
# `basepod version`).
#
# Builds only — it does not tag, push, or publish. Cutting the actual
# release and deploying to a server stay manual on purpose.
#
# Usage: scripts/release.sh v0.6.0
set -euo pipefail

VERSION="${1:-}"
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 vX.Y.Z (got: '${VERSION:-}')" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

OUT="$REPO_ROOT/dist"
PLATFORMS=(darwin/amd64 darwin/arm64 linux/amd64 linux/arm64)

echo "==> Building dashboard (embedded into the binary via go:embed)"
(cd web && npm run build)

echo "==> Preparing $OUT"
mkdir -p "$OUT"
# Only ever remove this version's own artifacts, so a stray file in dist/
# is never silently destroyed by a release build.
rm -f "$OUT/basepod_${VERSION}"_*.tar.gz "$OUT/checksums.txt"

for platform in "${PLATFORMS[@]}"; do
  goos="${platform%/*}"
  goarch="${platform#*/}"
  echo "==> Building $goos/$goarch"
  # CGO off: the sqlite driver is modernc.org/sqlite (pure Go), so every
  # target cross-compiles from any host without a C toolchain.
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" \
    -o "$OUT/basepod" ./cmd/basepod
  tar -czf "$OUT/basepod_${VERSION}_${goos}_${goarch}.tar.gz" -C "$OUT" basepod
  rm -f "$OUT/basepod"
done

echo "==> Generating checksums.txt"
(cd "$OUT" && shasum -a 256 "basepod_${VERSION}"_*.tar.gz > checksums.txt)

echo
echo "Artifacts in $OUT:"
(cd "$OUT" && ls -lh "basepod_${VERSION}"_*.tar.gz checksums.txt | awk '{print "  " $NF " (" $5 ")"}')
echo
echo "Next: gh release create $VERSION $OUT/basepod_${VERSION}_*.tar.gz $OUT/checksums.txt"
