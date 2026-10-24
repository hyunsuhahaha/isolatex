#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

# Build on Linux (including WSL2) or macOS. No releases are published here.
version="${1:-dev}"
output="${2:-dist}"
if [[ ! "$version" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo 'version must contain only letters, digits, dots, underscores and hyphens' >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  digest_command=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  digest_command=(shasum -a 256)
else
  echo 'building distributions requires sha256sum or shasum' >&2
  exit 1
fi
mkdir -p "$output"
dist_root="$(cd "$output" && pwd)"
commit="$(git rev-parse HEAD)"
if [[ -n "$(git status --porcelain)" ]]; then commit="$commit-dirty"; fi
build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.buildDate=$build_date"
archives=()

for target in ${TARGETS:-linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64}; do
  if [[ ! "$target" =~ ^(linux|windows|darwin)/(amd64|arm64)$ ]]; then
    echo "unsupported distribution target: $target" >&2
    exit 1
  fi
  goos="${target%/*}"
  goarch="${target#*/}"
  name="isolatex_${version}_${goos}_${goarch}"
  package="$dist_root/$name"
  suffix=''
  if [[ "$goos" == windows ]]; then suffix='.exe'; fi
  mkdir -p "$package/bin" "$package/docs" "$package/examples"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$package/bin/isolatex$suffix" ./cmd/isolatex
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -o "$package/bin/demo-app$suffix" ./examples/demo-app
  cp README.md LICENSE CONTRIBUTING.md SECURITY.md isolatex.yaml "$package/"
  cp docs/*.md "$package/docs/"
  cp examples/static.yaml "$package/examples/"
  cp -R examples/observability "$package/examples/"
  archive="$name.tar.gz"
  tar -czf "$dist_root/$archive" -C "$package" .
  archives+=("$archive")
  echo "$dist_root/$archive"
done
(cd "$dist_root" && "${digest_command[@]}" "${archives[@]}" > SHA256SUMS)
