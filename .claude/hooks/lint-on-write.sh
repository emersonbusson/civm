#!/usr/bin/env bash
# PostToolUse hook: runs Go tooling after file writes/edits.
# Non-blocking — reports issues but never blocks (exit 0 always).
#
# civm is a Go CLI (civmctl). We gofmt-check + go vet the touched package.
#
# Environment variables provided by Claude Code:
#   TOOL_INPUT - JSON string with the tool input (contains "file_path" field)

set -o pipefail

# Extract file_path from TOOL_INPUT JSON
FILE_PATH=$(echo "${TOOL_INPUT:-}" | grep -o '"file_path"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/"file_path"[[:space:]]*:[[:space:]]*"//;s/"$//' || true)

if [ -z "$FILE_PATH" ]; then
  exit 0
fi

# Only act on Go files
case "$FILE_PATH" in
  *.go) ;;
  *) exit 0 ;;
esac

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
[ -f "$REPO_ROOT/go.mod" ] || exit 0
cd "$REPO_ROOT" || exit 0

# gofmt: report files that need formatting
if command -v gofmt >/dev/null 2>&1; then
  UNFMT=$(gofmt -l "$FILE_PATH" 2>/dev/null || true)
  if [ -n "$UNFMT" ]; then
    echo "gofmt: needs formatting -> $UNFMT (run: gofmt -w $UNFMT)"
  fi
fi

# go vet on the package containing the file (best-effort, non-blocking)
PKG_DIR=$(dirname "$FILE_PATH")
if command -v go >/dev/null 2>&1 && [ -d "$PKG_DIR" ]; then
  go vet "./$(realpath --relative-to="$REPO_ROOT" "$PKG_DIR")" 2>&1 | tail -20 || true
fi

exit 0
