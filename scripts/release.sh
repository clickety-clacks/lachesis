#!/bin/sh
set -eu

usage() {
  echo "usage: $0 [--dry-run] vMAJOR.MINOR.PATCH" >&2
  exit 2
}

dry_run=false
if [ "${1:-}" = "--dry-run" ]; then
  dry_run=true
  shift
fi
[ "$#" -eq 1 ] || usage
tag=$1

if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "release tag must have the form vMAJOR.MINOR.PATCH" >&2
  exit 2
fi

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if ! commit=$(git -C "$repo_dir" rev-parse --verify "refs/tags/$tag^{commit}" 2>/dev/null); then
  echo "release tag does not exist locally: $tag" >&2
  exit 2
fi

release_dir=$(mktemp -d "${TMPDIR:-/tmp}/lachesis-release.XXXXXX")
cleanup() { rm -rf "$release_dir"; }
trap cleanup EXIT INT TERM
source_dir=$release_dir/source
mkdir "$source_dir"
git -C "$repo_dir" archive "$tag" | tar -x -C "$source_dir"

version=${tag#v}
ldflags="-X main.buildVersion=$version -X main.buildCommit=$commit"
build() {
  goos=$1
  goarch=$2
  output=$release_dir/lachesis-$goos-$goarch
  (
    cd "$source_dir"
    CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
      go build -buildvcs=false -trimpath -ldflags "$ldflags" -o "$output" ./cmd/lachesis
  )
}

build linux amd64
build darwin arm64
(
  cd "$release_dir"
  LC_ALL=C sha256sum lachesis-darwin-arm64 lachesis-linux-amd64 >SHA256SUMS
)

if [ "$dry_run" = true ]; then
  echo "dry-run: built $tag from $commit"
  cat "$release_dir/SHA256SUMS"
  echo "dry-run: would publish $tag with lachesis-linux-amd64, lachesis-darwin-arm64, and SHA256SUMS"
  exit 0
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "gh is required to publish a release" >&2
  exit 1
fi
gh release create "$tag" \
  "$release_dir/lachesis-linux-amd64" \
  "$release_dir/lachesis-darwin-arm64" \
  "$release_dir/SHA256SUMS" \
  --verify-tag \
  --generate-notes \
  --title "$tag"
