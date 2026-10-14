#!/usr/bin/env bash
# Rename the Go module path before publishing, e.g.:
#   ./scripts/set-module.sh github.com/YOUR_USER/lumen
# Rewrites go.mod, all imports and the build flags that reference the path, then checks the build.
set -euo pipefail
cd "$(dirname "$0")/.."
NEW="${1:-}"
[[ "$NEW" =~ ^[A-Za-z0-9._~/-]+$ ]] || { echo "usage: $0 github.com/USER/REPO" >&2; exit 1; }
OLD="$(sed -n 's/^module //p' go.mod)"
[ "$OLD" != "$NEW" ] || { echo "already $NEW"; exit 0; }
grep -rlF --include='*.go' --include='go.mod' --include='Dockerfile*' --include='*.md' "$OLD" . | while read -r f; do
  sed -i.bak "s#$OLD#$NEW#g" "$f" && rm -f "$f.bak"
done
echo "module is now $NEW"
go build ./... && echo "build OK"
