#!/usr/bin/env bash
# PreToolUse hook: blocks dangerous bash commands before execution.
# Exit 0 = allow, Exit 2 = block.
#
# civm-specific note: `sudo` IS allowed here on purpose — civm legitimately runs
# `sudo civmctl bootstrap`, `sudo systemctl`, etc. We do NOT blanket-block sudo
# (unlike application repos). Destructive filesystem ops are still blocked, even
# when prefixed with sudo (the rm/-rf checks scan the whole command).
#
# Environment variables provided by Claude Code:
#   TOOL_INPUT - JSON string with the tool input (contains "command" field for Bash)

# Do NOT use set -e here: grep returns 1 on no-match, which would false-trigger exit.

extract_command() {
  if command -v jq >/dev/null 2>&1; then
    printf '%s' "${TOOL_INPUT:-}" | jq -r '.command // empty' 2>/dev/null
    return $?
  fi

  if command -v python3 >/dev/null 2>&1; then
    TOOL_INPUT_JSON="${TOOL_INPUT:-}" python3 - <<'PY'
import json
import os
import sys

raw = os.environ.get("TOOL_INPUT_JSON", "")
if not raw:
    sys.exit(0)

try:
    payload = json.loads(raw)
except json.JSONDecodeError:
    sys.exit(3)

command = payload.get("command", "")
if not isinstance(command, str):
    sys.exit(3)

sys.stdout.write(command)
PY
    return $?
  fi

  if command -v node >/dev/null 2>&1; then
    TOOL_INPUT_JSON="${TOOL_INPUT:-}" node -e '
      const raw = process.env.TOOL_INPUT_JSON ?? "";
      if (!raw) {
        process.exit(0);
      }

      let payload;
      try {
        payload = JSON.parse(raw);
      } catch {
        process.exit(3);
      }

      if (typeof payload.command !== "string" && payload.command !== undefined) {
        process.exit(3);
      }

      process.stdout.write(payload.command ?? "");
    '
    return $?
  fi

  return 4
}

COMMAND=""
if ! COMMAND="$(extract_command)"; then
  status=$?
  if [ -n "${TOOL_INPUT:-}" ] && [ "$status" -ne 0 ]; then
    echo "BLOCKED: unable to parse TOOL_INPUT JSON safely without jq" >&2
    exit 2
  fi
fi

if [ -z "$COMMAND" ]; then
  exit 0
fi

# Block destructive filesystem operations (also catches `sudo rm -rf /`)
if echo "$COMMAND" | grep -qE 'rm\s+(-[a-zA-Z]*f[a-zA-Z]*\s+)?/($|\s)'; then
  echo "BLOCKED: refusing to delete root filesystem" >&2
  exit 2
fi

if echo "$COMMAND" | grep -qE 'rm\s+-rf\s+/'; then
  echo "BLOCKED: refusing rm -rf on root paths" >&2
  exit 2
fi

# Block chmod 777 (any flags / octal variants: chmod -R 777, chmod 0777)
if echo "$COMMAND" | grep -qE 'chmod\s+(-[a-zA-Z]+\s+)*0?777'; then
  echo "BLOCKED: chmod 777 not allowed" >&2
  exit 2
fi

# Block writes to raw block devices (dd / mkfs / shell redirect). Destructive and
# never a legit Claude command in civm — civmctl does its own device ops itself.
# (scan the whole command, so a sudo prefix does not get past these)
if echo "$COMMAND" | grep -qE '\bdd\b[^|]*\bof=/dev/'; then
  echo "BLOCKED: refusing dd to a block device" >&2
  exit 2
fi
if echo "$COMMAND" | grep -qE '\bmkfs(\.[a-z0-9]+)?\b'; then
  echo "BLOCKED: refusing to format a filesystem (mkfs)" >&2
  exit 2
fi
if echo "$COMMAND" | grep -qE '>\s*/dev/(sd|nvme|vd|mapper)'; then
  echo "BLOCKED: refusing to overwrite a block device" >&2
  exit 2
fi

# Block fork bomb (:(){ :|:& };:)
if echo "$COMMAND" | grep -qE ':\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:'; then
  echo "BLOCKED: fork bomb pattern" >&2
  exit 2
fi

# Block direct reads of .env / credential files (allow .env.example, .env.test)
if echo "$COMMAND" | grep -qE '(cat|less|more|head|tail|bat)\s+.*\.env($|\s)'; then
  if ! echo "$COMMAND" | grep -qE '\.env\.(example|test|sample)'; then
    echo "BLOCKED: reading .env files directly is not allowed (use .env.example)" >&2
    exit 2
  fi
fi

# Block committing .env files
if echo "$COMMAND" | grep -qE 'git\s+add\s+.*\.env($|\s)'; then
  if ! echo "$COMMAND" | grep -qE '\.env\.(example|test|sample)'; then
    echo "BLOCKED: staging .env files is not allowed" >&2
    exit 2
  fi
fi

exit 0
