#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT_DIR=$(cd -- "$SCRIPT_DIR/../.." && pwd)
LOCK_FILE="$SCRIPT_DIR/lock.json"
CACHE_DIR="$ROOT_DIR/dist/runtime-cache"
OUTPUT_DIR="$ROOT_DIR/dist/codex"
version=$(jq -er '.version' "$LOCK_FILE")
archive_url=$(jq -er '.archive.url' "$LOCK_FILE")
archive_hash=$(jq -er '.archive.sha256' "$LOCK_FILE")
archive_file="$CACHE_DIR/codex-$version-linux-amd64.tar.gz"
mkdir -p "$CACHE_DIR"
stage_dir=$(mktemp -d "$CACHE_DIR/extract.XXXXXX")
trap 'rm -rf -- "$stage_dir"' EXIT
verify_archive() { printf '%s  %s\n' "$archive_hash" "$archive_file" | sha256sum --check --status; }
if [[ ! -f "$archive_file" ]] || ! verify_archive; then
  curl --fail --location --silent --show-error --proto '=https' --connect-timeout 15 \
    --max-time 300 --retry 2 --retry-delay 1 --retry-max-time 600 \
    --output "$stage_dir/archive.tar.gz" "$archive_url"
  printf '%s  %s\n' "$archive_hash" "$stage_dir/archive.tar.gz" | sha256sum --check
  mv -- "$stage_dir/archive.tar.gz" "$archive_file"
fi
verify_archive
mkdir "$stage_dir/package"
tar -xzf "$archive_file" -C "$stage_dir/package"
# Keep the complete upstream distribution, including rg and runtime resources.
while IFS= read -r binary_name; do
  binary_file="$stage_dir/package/bin/$binary_name"
  [[ -x "$binary_file" ]] || { printf 'Upstream bundle is missing %s\n' "$binary_name" >&2; exit 1; }
  expected_hash=$(jq -er --arg name "$binary_name" '.binaries[$name]' "$LOCK_FILE")
  printf '%s  %s\n' "$expected_hash" "$binary_file" | sha256sum --check
done < <(jq -r '.binaries | keys[]' "$LOCK_FILE")
rm -rf -- "$OUTPUT_DIR"
mv -- "$stage_dir/package" "$OUTPUT_DIR"
printf 'Platform Codex %s installed in %s\n' "$version" "$OUTPUT_DIR"
