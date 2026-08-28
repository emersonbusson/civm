#!/usr/bin/env bash
#
# configure-paid-ci-environment.sh — enable protected paid CI for a peer repo.
#
# Creates or updates the paid-github-hosted-ci environment with required
# reviewers and prevent-self-review. With --enable, also sets the repository
# variable ENABLE_PAID_GITHUB_HOSTED_CI=true after verification succeeds.

set -euo pipefail

usage() {
  sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'
  cat <<'USAGE'

Usage:
  scripts/configure-paid-ci-environment.sh \
    --repo owner/repo \
    --reviewer-login admin1 \
    --reviewer-login admin2 \
    [--environment paid-github-hosted-ci] \
    [--enable]

Rollback:
  gh variable set ENABLE_PAID_GITHUB_HOSTED_CI --body false --repo owner/repo
USAGE
}

REPO=""
ENVIRONMENT="paid-github-hosted-ci"
ENABLE=false
REVIEWER_LOGINS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --repo)
      REPO="$2"
      shift 2
      ;;
    --environment)
      ENVIRONMENT="$2"
      shift 2
      ;;
    --reviewer-login)
      REVIEWER_LOGINS+=("$2")
      shift 2
      ;;
    --enable)
      ENABLE=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown arg: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [ -z "$REPO" ]; then
  echo "ERROR: --repo is required" >&2
  usage >&2
  exit 2
fi

if [ "${#REVIEWER_LOGINS[@]}" -eq 0 ]; then
  echo "ERROR: at least one --reviewer-login is required" >&2
  usage >&2
  exit 2
fi

if [ "${#REVIEWER_LOGINS[@]}" -gt 6 ]; then
  echo "ERROR: GitHub supports at most 6 required reviewers per environment" >&2
  exit 2
fi

command -v gh >/dev/null 2>&1 || { echo "ERROR: gh is required" >&2; exit 2; }
command -v jq >/dev/null 2>&1 || { echo "ERROR: jq is required" >&2; exit 2; }

reviewers_json="[]"
for login in "${REVIEWER_LOGINS[@]}"; do
  id="$(gh api "users/$login" --jq .id)"
  reviewers_json="$(
    jq -c --argjson id "$id" \
      '. + [{"type":"User","id":$id}]' <<< "$reviewers_json"
  )"
done

body="$(
  jq -n --argjson reviewers "$reviewers_json" '{
    wait_timer: 0,
    prevent_self_review: true,
    reviewers: $reviewers,
    deployment_branch_policy: null
  }'
)"

echo "Configuring environment $ENVIRONMENT for $REPO"
existing_environment=false
if gh api "repos/$REPO/environments/$ENVIRONMENT" >/dev/null 2>&1; then
  existing_environment=true
fi

set +e
put_output="$(gh api -X PUT "repos/$REPO/environments/$ENVIRONMENT" --input - <<< "$body" 2>&1)"
put_status=$?
set -e

if [ "$put_status" -ne 0 ]; then
  printf '%s\n' "$put_output" >&2
  if ! $existing_environment; then
    gh api -X DELETE "repos/$REPO/environments/$ENVIRONMENT" >/dev/null 2>&1 || true
  fi
  exit "$put_status"
fi

env_json="$(gh api "repos/$REPO/environments/$ENVIRONMENT")"
if ! jq -e '
  .protection_rules[]?
  | select(.type == "required_reviewers")
  | select(.prevent_self_review == true)
  | select((.reviewers // []) | length > 0)
' <<< "$env_json" >/dev/null; then
  echo "ERROR: environment was created without required-reviewer protection" >&2
  echo "       Do not enable paid CI until the GitHub plan supports this rule." >&2
  if ! $existing_environment; then
    gh api -X DELETE "repos/$REPO/environments/$ENVIRONMENT" >/dev/null 2>&1 || true
  fi
  exit 1
fi

echo "Environment protection verified"

if $ENABLE; then
  gh variable set ENABLE_PAID_GITHUB_HOSTED_CI \
    --body true \
    --repo "$REPO" >/dev/null
  echo "Repository variable ENABLE_PAID_GITHUB_HOSTED_CI=true"
else
  echo "Paid CI variable left unchanged. Re-run with --enable when ready."
fi
