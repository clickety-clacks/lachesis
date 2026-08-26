#!/bin/sh
set -eu

github_repo=clickety-clacks/lachesis

usage() {
  echo "usage: $0 [--dry-run] vMAJOR.MINOR.PATCH" >&2
  exit 2
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "required command not found: $1" >&2
    exit 1
  fi
}

dry_run=false
if [ "${1:-}" = "--dry-run" ]; then
  dry_run=true
  shift
fi
[ "$#" -eq 1 ] || usage
tag=$1

require_command grep
if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "release tag must have the form vMAJOR.MINOR.PATCH" >&2
  exit 2
fi

require_command dirname
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
for tool in git mktemp mkdir tar go sha256sum cat rm; do
  require_command "$tool"
done
if [ "$dry_run" = false ]; then
  require_command gh
fi

if commit=$(git -C "$repo_dir" rev-parse --verify "refs/tags/$tag^{commit}" 2>/dev/null); then
  :
else
  status=$?
  if [ "$status" -eq 126 ] || [ "$status" -eq 127 ]; then
    echo "required command failed: git" >&2
    exit 1
  fi
  echo "release tag does not exist locally: $tag" >&2
  exit 2
fi

release_dir=$(mktemp -d "${TMPDIR:-/tmp}/lachesis-release.XXXXXX")
cleanup() { rm -rf "$release_dir"; }
trap cleanup EXIT INT TERM
source_dir=$release_dir/source
archive_file=$release_dir/source.tar
mkdir "$source_dir"
git -C "$repo_dir" archive --format=tar --output="$archive_file" "$commit"
tar -xf "$archive_file" -C "$source_dir"
rm "$archive_file"

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
  echo "dry-run: would publish $tag to $github_repo with lachesis-linux-amd64, lachesis-darwin-arm64, and SHA256SUMS"
  exit 0
fi

if current_commit=$(git -C "$repo_dir" rev-parse --verify "refs/tags/$tag^{commit}" 2>/dev/null); then
  :
else
  status=$?
  if [ "$status" -eq 126 ] || [ "$status" -eq 127 ]; then
    echo "required command failed: git" >&2
    exit 1
  fi
  echo "release tag no longer exists locally: $tag" >&2
  exit 1
fi
if [ "$current_commit" != "$commit" ]; then
  echo "release tag changed while building: $tag" >&2
  exit 1
fi

gh release create "$tag" \
  "$release_dir/lachesis-linux-amd64" \
  "$release_dir/lachesis-darwin-arm64" \
  "$release_dir/SHA256SUMS" \
  --verify-tag \
  --generate-notes \
  --title "$tag" \
  --repo "$github_repo"
