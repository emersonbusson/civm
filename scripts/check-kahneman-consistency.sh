#!/usr/bin/env bash
#
# check-kahneman-consistency.sh — cross-repo audit of the Kahneman discipline pattern.
#
# Reads disciplines/kahneman-sync-manifest.json (schema v2) and asserts, for
# every authorized fork, that:
#
#   1. The repo directory exists at <base>/<repo.path>.
#   2. The Kahneman discipline doc exists at <repo>/<repo.doc_path>.
#   3. The doc's first-line title matches the whitelist for repo.title.
#   4. The doc body has 12 named disciplines (### N. Name) — except civm,
#      whose template uses tables and accepts 0 or 12.
#   5. Each declared surface (CLAUDE.md / AGENTS.md) exists.
#   6. Each surface satisfies its declared style:
#        - h2_top5     : has "## Decision hygiene (Kahneman)" H2 header,
#                        the section links to the doc, universal framing is
#                        present, exactly 5 numbered rules, and rule 5 matches
#                        the governance variant declared in the manifest.
#        - inline_bold : has "**Decision hygiene (Kahneman) — always on" bold
#                        preamble, references the doc, and contains the
#                        universal framing.
#
# Usage:
#   ./scripts/check-kahneman-consistency.sh
#   ./scripts/check-kahneman-consistency.sh --json
#   ./scripts/check-kahneman-consistency.sh --strict        # warns -> failures
#   ./scripts/check-kahneman-consistency.sh --base <path>   # override $HOME/codespace
#   ./scripts/check-kahneman-consistency.sh --manifest <f>  # override manifest path
#   ./scripts/check-kahneman-consistency.sh --sync-missing  # clone missing repos from manifest git_url
#
# Exit codes:
#   0   all checks pass (warns allowed unless --strict)
#   1   one or more failures (or warns under --strict)
#   2   harness error (missing jq, missing manifest, bad args, malformed JSON)

set -euo pipefail

#--------------------------------- args / config -------------------------------

usage() {
  sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//'
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE="${KAHNEMAN_BASE:-$HOME/codespace}"
MANIFEST="$SCRIPT_DIR/../disciplines/kahneman-sync-manifest.json"
JSON_MODE=false
STRICT=false
SYNC_MISSING=false

while [ $# -gt 0 ]; do
  case "$1" in
    --json)         JSON_MODE=true; shift ;;
    --strict)       STRICT=true; shift ;;
    --sync-missing) SYNC_MISSING=true; shift ;;
    --base)         BASE="$2"; shift 2 ;;
    --manifest)     MANIFEST="$2"; shift 2 ;;
    -h|--help)      usage; exit 0 ;;
    *)              echo "ERROR: unknown arg: $1" >&2; usage >&2; exit 2 ;;
  esac
done

command -v jq >/dev/null 2>&1 || { echo "ERROR: jq is required (https://jqlang.github.io/jq/)" >&2; exit 2; }
[ -f "$MANIFEST" ]            || { echo "ERROR: manifest not found: $MANIFEST" >&2; exit 2; }
if $SYNC_MISSING; then
  command -v git >/dev/null 2>&1 || { echo "ERROR: git is required for --sync-missing" >&2; exit 2; }
fi

if ! jq -e . "$MANIFEST" >/dev/null 2>&1; then
  echo "ERROR: manifest is not valid JSON: $MANIFEST" >&2
  exit 2
fi

schema=$(jq -r '.schemaVersion // 0' "$MANIFEST")
if [ "$schema" != "2" ]; then
  echo "ERROR: manifest schemaVersion=$schema, this script requires schemaVersion=2" >&2
  exit 2
fi

#--------------------------------- load manifest -------------------------------

mapfile -t REPO_NAMES < <(jq -r '.repos[].name' "$MANIFEST")
if [ "${#REPO_NAMES[@]}" -eq 0 ]; then
  echo "ERROR: manifest has zero repos" >&2
  exit 2
fi

# Framing variants are literal phrases (no regex metacharacters); concatenate
# into a grep -E alternation.
FRAMING_RE="$(jq -r '.framing_variants | join("|")' "$MANIFEST")"

#--------------------------------- helpers -------------------------------------

# Extract the body of §"Decision hygiene (Kahneman)" — lines between that H2
# and the next H2. Empty output means header not present.
extract_h2_section() {
  awk '
    /^## Decision hygiene \(Kahneman\)/ { in_section = 1; next }
    /^## /                              { if (in_section) in_section = 0 }
    in_section                          { print }
  ' "$1"
}

# Extract the inline-bold preamble paragraph (from the marker line until the
# next blank line). Empty output means marker not present.
extract_inline_bold() {
  awk '
    /^\*\*Decision hygiene \(Kahneman\) — always on/ { in_block = 1 }
    in_block                                          { print }
    in_block && /^[[:space:]]*$/                      { exit }
  ' "$1"
}

# Count rule 5 occurrences and capture the line (first match).
get_rule5() {
  grep -E '^5\. \*\*' <<< "$1" | head -1 || true
}

repo_slug_from_url() {
  sed -E \
    -e 's#^https://github.com/##' \
    -e 's#^git@github.com:##' \
    -e 's#\.git$##' \
    <<< "$1"
}

sync_missing_repo() {
  local name="$1"
  local repo_full="$2"
  local git_url="$3"
  shift 3

  [ -d "$repo_full" ] && return 0
  $SYNC_MISSING || return 0
  [ -n "$git_url" ] && [ "$git_url" != "null" ] || return 0

  mkdir -p "$(dirname "$repo_full")"
  local slug
  slug="$(repo_slug_from_url "$git_url")"
  echo "sync: cloning $name from $slug" >&2

  if command -v gh >/dev/null 2>&1 && gh auth status -h github.com >/dev/null 2>&1; then
    gh repo clone "$slug" "$repo_full" -- \
      --filter=blob:none --sparse --depth=1 --quiet >/dev/null 2>&1 || true
  else
    git clone --filter=blob:none --sparse --depth=1 --quiet "$git_url" "$repo_full" >/dev/null 2>&1 || true
  fi

  if [ -d "$repo_full/.git" ] && [ "$#" -gt 0 ]; then
    git -C "$repo_full" sparse-checkout set --no-cone "$@" >/dev/null 2>&1 || true
  fi
}

#--------------------------------- main loop -----------------------------------

declare -A STATUS    # repo_name -> ok|warn|fail
declare -A REASONS   # repo_name -> "; "-joined reasons
ok=0; warn=0; fail=0

bump() {
  local repo="$1" level="$2" reason="$3"
  local cur="${STATUS[$repo]:-ok}"
  case "$level" in
    fail)                          STATUS[$repo]="fail" ;;
    warn) [ "$cur" != "fail" ] && STATUS[$repo]="warn" ;;
  esac
  if [ -n "${REASONS[$repo]:-}" ]; then
    REASONS[$repo]="${REASONS[$repo]}; $reason"
  else
    REASONS[$repo]="$reason"
  fi
}

for name in "${REPO_NAMES[@]}"; do
  STATUS[$name]="ok"
  REASONS[$name]=""

  repo_cfg=$(jq -c --arg n "$name" '.repos[] | select(.name==$n)' "$MANIFEST")
  repo_path=$(jq -r '.path'        <<< "$repo_cfg")
  git_url=$(  jq -r '.git_url // empty' <<< "$repo_cfg")
  doc_rel=$(  jq -r '.doc_path'    <<< "$repo_cfg")
  title_key=$(jq -r '.title'       <<< "$repo_cfg")
  mapfile -t sparse_paths < <(jq -r '.doc_path, .surfaces[].file' <<< "$repo_cfg" | sort -u)

  expected_title=$(jq -r --arg k "$title_key" '.title_patterns[$k] // empty' "$MANIFEST")
  if [ -z "$expected_title" ]; then
    bump "$name" fail "manifest: unknown title key '$title_key'"
    continue
  fi

  repo_full="$BASE/$repo_path"
  doc_full="$repo_full/$doc_rel"

  sync_missing_repo "$name" "$repo_full" "$git_url" "${sparse_paths[@]}"

  # ---- Check 1: repo dir exists --------------------------------------------
  if [ ! -d "$repo_full" ]; then
    bump "$name" fail "repo dir missing: $repo_full"
    continue
  fi

  # ---- Check 2: doc exists at canonical path -------------------------------
  if [ ! -f "$doc_full" ]; then
    bump "$name" fail "doc missing: $doc_rel"
  else
    # ---- Check 3: title in whitelist ---------------------------------------
    actual_title=$(head -1 "$doc_full" | tr -d '\r')
    if [ "$actual_title" != "$expected_title" ]; then
      bump "$name" warn "title drift: expected '$expected_title' got '$actual_title'"
    fi

    # ---- Check 4: 13 disciplines in body -----------------------------------
    discipline_count=$(grep -cE '^### [0-9]+\. ' "$doc_full" || true)
    if [ "$name" = "civm" ]; then
      # civm template uses tables, not H3 headings — accept 0 or 13.
      if [ "$discipline_count" -ne 0 ] && [ "$discipline_count" -ne 13 ]; then
        bump "$name" warn "civm: $discipline_count H3 disciplines (expected 0 or 13)"
      fi
    elif [ "$discipline_count" -ne 13 ]; then
      bump "$name" fail "doc has $discipline_count disciplines (expected 13)"
    fi
  fi

  # ---- Per-surface checks --------------------------------------------------
  mapfile -t surfaces < <(jq -c '.surfaces[]' <<< "$repo_cfg")

  for surf_json in "${surfaces[@]}"; do
    surf_file=$(jq -r  '.file'   <<< "$surf_json")
    surf_style=$(jq -r '.style'  <<< "$surf_json")
    rule5_key=$(jq -r  '.rule5 // empty' <<< "$surf_json")

    sf="$repo_full/$surf_file"
    if [ ! -f "$sf" ]; then
      bump "$name" fail "$surf_file missing"
      continue
    fi

    case "$surf_style" in
      h2_top5)
        if ! grep -qE '^## Decision hygiene \(Kahneman\)' "$sf"; then
          bump "$name" fail "$surf_file: §\"Decision hygiene (Kahneman)\" H2 header missing"
          continue
        fi

        section="$(extract_h2_section "$sf")"
        if [ -z "$section" ]; then
          bump "$name" fail "$surf_file: H2 section body empty"
          continue
        fi

        # Framing and doc link must live inside the section, not just somewhere in the file —
        # this catches regressions where the section is refactored but redundant copies elsewhere mask the loss.
        if ! grep -qE "$FRAMING_RE" <<< "$section"; then
          bump "$name" fail "$surf_file: universal framing missing inside §\"Decision hygiene\""
        fi
        if ! grep -qF "$doc_rel" <<< "$section"; then
          bump "$name" fail "$surf_file: §\"Decision hygiene\" does not link to $doc_rel"
        fi

        rule_count=$(grep -cE '^[1-9]\. \*\*' <<< "$section" || true)
        if [ "$rule_count" -ne 5 ]; then
          bump "$name" warn "$surf_file: $rule_count numbered rules (expected 5)"
        fi

        if [ -n "$rule5_key" ]; then
          expected_rule5=$(jq -r --arg k "$rule5_key" '.rule5_patterns[$k] // empty' "$MANIFEST")
          if [ -z "$expected_rule5" ]; then
            bump "$name" fail "$surf_file: manifest unknown rule5 key '$rule5_key'"
          else
            rule5_line="$(get_rule5 "$section")"
            if [ -z "$rule5_line" ]; then
              bump "$name" warn "$surf_file: rule 5 not found"
            elif ! grep -qF "$expected_rule5" <<< "$rule5_line"; then
              bump "$name" warn "$surf_file: rule 5 doesn't contain '$expected_rule5'"
            fi
          fi
        fi
        ;;

      inline_bold)
        if ! grep -qE '^\*\*Decision hygiene \(Kahneman\) — always on' "$sf"; then
          bump "$name" fail "$surf_file: inline_bold marker missing"
          continue
        fi
        block="$(extract_inline_bold "$sf")"
        if [ -z "$block" ]; then
          bump "$name" fail "$surf_file: inline_bold block body empty"
          continue
        fi

        if ! grep -qE "$FRAMING_RE" <<< "$block"; then
          bump "$name" fail "$surf_file: universal framing missing inside inline_bold block"
        fi
        if ! grep -qF "$doc_rel" <<< "$block"; then
          bump "$name" fail "$surf_file: inline_bold block does not link to $doc_rel"
        fi
        ;;

      *)
        bump "$name" fail "$surf_file: manifest unknown style '$surf_style'"
        ;;
    esac
  done
done

# Tally
for name in "${REPO_NAMES[@]}"; do
  case "${STATUS[$name]}" in
    ok)   ok=$((ok+1))   ;;
    warn) warn=$((warn+1)) ;;
    fail) fail=$((fail+1)) ;;
  esac
done

#--------------------------------- output --------------------------------------

if $JSON_MODE; then
  rows_json="$(
    for name in "${REPO_NAMES[@]}"; do
      jq -n \
        --arg n  "$name" \
        --arg s  "${STATUS[$name]}" \
        --arg rr "${REASONS[$name]}" \
        '{name: $n,
          status: $s,
          reasons: (if $rr == "" then [] else ($rr | split("; ")) end)}'
    done | jq -s '.'
  )"
  jq -n \
    --argjson ok    "$ok" \
    --argjson warn  "$warn" \
    --argjson fail  "$fail" \
    --argjson rows  "$rows_json" \
    --arg     base  "$BASE" \
    --arg     mani  "$MANIFEST" \
    '{summary: {total: ($ok + $warn + $fail), ok: $ok, warn: $warn, fail: $fail},
      base: $base,
      manifest: $mani,
      repos: $rows}'
else
  for name in "${REPO_NAMES[@]}"; do
    case "${STATUS[$name]}" in
      ok)   icon="✓" ;;
      warn) icon="⚠" ;;
      fail) icon="✗" ;;
    esac
    if [ -z "${REASONS[$name]}" ]; then
      printf '%s %s\n' "$icon" "$name"
    else
      printf '%s %s — %s\n' "$icon" "$name" "${REASONS[$name]}"
    fi
  done
  echo '---'
  echo "Total: $((ok+warn+fail)) | ok: $ok | warn: $warn | fail: $fail"
fi

# Exit codes
[ "$fail" -gt 0 ]                && exit 1
$STRICT && [ "$warn" -gt 0 ]     && exit 1
exit 0
