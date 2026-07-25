#!/usr/bin/env bash
# Account-wide Dependabot + GitHub Actions CI audit. READ-ONLY.
#
# Emits one TSV record per repo with findings, then a summary block.
# Never writes: no merges, no commits, no archive toggling, no run deletion.
#
# Usage:
#   ./audit-repos.sh [owner]          # defaults to the authenticated user
#   ./audit-repos.sh tiennm99
#   INCLUDE_FORKS=1 ./audit-repos.sh  # forks are excluded by default
#
# Repos listed in config/expected-dependabot-disabled.txt have their Dependabot
# findings waived: alerts report DISABLED_OK, and leftover updater check-run
# failures stop counting. Their own build failures still count. Override the path
# with EXPECTED_DISABLED_FILE=/some/file, or /dev/null to waive nothing.
#
# Output columns:
#   repo  archived  dependabot_prs  other_prs  alerts  ci_state  detail

set -uo pipefail

OWNER="${1:-$(gh api user --jq .login)}"
INCLUDE_FORKS="${INCLUDE_FORKS:-0}"
LIMIT="${REPO_LIMIT:-1000}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXPECTED_DISABLED_FILE="${EXPECTED_DISABLED_FILE:-$SCRIPT_DIR/../config/expected-dependabot-disabled.txt}"

command -v gh >/dev/null || { echo "ERROR: gh CLI not found" >&2; exit 1; }
command -v jq >/dev/null || { echo "ERROR: jq not found" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "ERROR: gh not authenticated (run: gh auth login)" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ---------------------------------------------------------------------------
# 0. Repos whose Dependabot checks are intentionally waived.
#    Strip comments, CR (the file may be edited on Windows) and blank lines.
#    A waiver suppresses Dependabot-state findings only: the alerts check and
#    leftover updater check-run failures. The repo's own build failures, stuck
#    third-party statuses and open Dependabot PRs are still reported, so real
#    breakage on a waived repo never disappears.
# ---------------------------------------------------------------------------
: > "$WORK/waived.txt"
if [ -f "$EXPECTED_DISABLED_FILE" ]; then
  sed 's/#.*//' "$EXPECTED_DISABLED_FILE" | tr -d '\r' | awk 'NF{print $1}' > "$WORK/waived.txt"
fi
N_WAIVED_CONFIGURED=$(wc -l < "$WORK/waived.txt" | tr -d ' ')
is_waived() { [ -s "$WORK/waived.txt" ] && grep -qxF "$1" "$WORK/waived.txt"; }

# ---------------------------------------------------------------------------
# 1. Repo inventory. Forks excluded by default: their alerts and Dependabot
#    PRs belong to the upstream project, not to this owner.
#
#    defaultBranchRef is fetched here so the per-repo "get default branch" call
#    can be skipped later. It is null on a repo with no commits.
# ---------------------------------------------------------------------------
gh repo list "$OWNER" --limit "$LIMIT" --json name,isFork,isArchived,defaultBranchRef \
  --jq '.[]|[.name,(.isFork|tostring),(.isArchived|tostring),(.defaultBranchRef.name // "")]|@tsv' \
  > "$WORK/repos.tsv"

if [ "$INCLUDE_FORKS" != "1" ]; then
  awk -F'\t' '$2=="false"' "$WORK/repos.tsv" > "$WORK/scope.tsv"
else
  cp "$WORK/repos.tsv" "$WORK/scope.tsv"
fi

TOTAL=$(wc -l < "$WORK/scope.tsv" | tr -d ' ')
N_ARCH=$(awk -F'\t' '$3=="true"' "$WORK/scope.tsv" | wc -l | tr -d ' ')
N_ACTIVE=$(awk -F'\t' '$3=="false"' "$WORK/scope.tsv" | wc -l | tr -d ' ')
N_FORKS=$(awk -F'\t' '$2=="true"' "$WORK/repos.tsv" | wc -l | tr -d ' ')

# ---------------------------------------------------------------------------
# 2. All open PRs in ONE search call rather than one per repo.
#    On hundreds of repos this is the difference between 1 call and N.
#
#    Author logins are NORMALIZED before matching. The search API (REST) reports
#    the bot as "dependabot[bot]"; gh's GraphQL-backed commands (gh pr list)
#    report the same bot as "app/dependabot". Matching only one form silently
#    counts every Dependabot PR as a human PR -- and a repo whose sole finding
#    is Dependabot PRs then emits no row at all, i.e. reads as clean.
#    Do NOT switch to the JSON `is_bot` field: gh 2.92.0 reports is_bot=false
#    for authors it simultaneously types as "Bot".
# ---------------------------------------------------------------------------
gh search prs --owner "$OWNER" --state open --limit 1000 \
  --json repository,number,title,author \
  --jq '.[]|[.repository.name,(.number|tostring),(.author.login|ltrimstr("app/")|rtrimstr("[bot]")),.title]|@tsv' \
  > "$WORK/prs.tsv" 2>/dev/null || : > "$WORK/prs.tsv"

DBOT_RE='^dependabot(-preview)?$'
dpr_count() { awk -F'\t' -v r="$1" -v re="$DBOT_RE" '$1==r && $3 ~ re' "$WORK/prs.tsv" | wc -l | tr -d ' '; }
opr_count() { awk -F'\t' -v r="$1" -v re="$DBOT_RE" '$1==r && $3 !~ re' "$WORK/prs.tsv" | wc -l | tr -d ' '; }

# ---------------------------------------------------------------------------
# 3. Dependabot alerts.
#    Three distinct outcomes that must NOT be collapsed into "0":
#      - array          -> real count
#      - 403 archived   -> UNREADABLE (unknown, NOT zero)
#      - 403 disabled   -> DISABLED   (nobody is looking, NOT zero)
# ---------------------------------------------------------------------------
#    --paginate is required: a repo can exceed one 100-item page, and a silent
#    truncation to 100 would understate exposure. Deliberately NO --jq here --
#    gh only copies an error response BODY to stdout when no filter is set, and
#    that body is what the archived/disabled classification below greps.
alerts_for() {
  local repo="$1" archived="$2" out merged
  if [ "$archived" = "true" ]; then
    echo -e "UNREADABLE\t"          # archived repos always 403; skip the call
    return
  fi
  # Checked AFTER archived so a waived repo that is also archived still counts
  # toward the archived blind spot rather than looking deliberately accepted.
  if is_waived "$repo"; then
    echo -e "DISABLED_OK\tnot checked: alerts disabled by intent"
    return
  fi
  out=$(gh api --paginate "repos/$OWNER/$repo/dependabot/alerts?state=open&per_page=100" 2>/dev/null)
  # An empty response must never be read as "0 alerts" -- that is unknown.
  if [ -z "${out//[[:space:]]/}" ]; then
    echo -e "ERROR\t"
    return
  fi
  # --paginate emits one JSON array per page; -s add concatenates them.
  merged=$(printf '%s' "$out" | jq -s 'add // []' 2>/dev/null || echo 'null')
  if echo "$merged" | jq -e 'type=="array"' >/dev/null 2>&1; then
    local n sev
    n=$(echo "$merged" | jq 'length')
    sev=$(echo "$merged" | jq -r '[.[]|"\(.security_advisory.severity):\(.dependency.package.name)"]|unique|join(", ")')
    echo -e "${n}\t${sev}"
  elif echo "$out" | grep -qi "archived"; then
    echo -e "UNREADABLE\t"
  elif echo "$out" | grep -qi "disabled"; then
    echo -e "DISABLED\t"
  else
    echo -e "ERROR\t"
  fi
}

# ---------------------------------------------------------------------------
# 4. CI state of the LATEST COMMIT only.
#    Historical run failures are noise -- a repo whose HEAD is green is healthy
#    regardless of what failed months ago.
#    Covers both check-runs (Actions) and commit statuses (Vercel, Cloudflare).
#    Dependabot's own updater check-runs are counted SEPARATELY from app CI:
#    they fail for dependency reasons, not because the build is broken.
# ---------------------------------------------------------------------------
#    The default branch arrives from the repo inventory, so no extra API call is
#    needed here. Empty (no commits) means there is nothing to judge.
ci_for() {
  local repo="$1" br="$2" sha runs statuses app_fail app_other dep_fail all
  [ -z "$br" ] && { echo -e "NO_COMMITS\t"; return; }
  sha=$(gh api "repos/$OWNER/$repo/commits/$br" --jq .sha 2>/dev/null) || { echo -e "ERROR\t"; return; }

  runs=$(gh api "repos/$OWNER/$repo/commits/$sha/check-runs" \
          --jq '[.check_runs[]|{n:.name,c:(.conclusion // .status)}]' 2>/dev/null)
  statuses=$(gh api "repos/$OWNER/$repo/commits/$sha/status" \
          --jq '[.statuses[]|{n:.context,c:.state}]' 2>/dev/null)
  # Guard against empty/non-JSON responses before combining.
  echo "$runs"     | jq -e 'type=="array"' >/dev/null 2>&1 || runs='[]'
  echo "$statuses" | jq -e 'type=="array"' >/dev/null 2>&1 || statuses='[]'

  # NOTE: process substitution (<(...)) is unreliable on Windows Git Bash
  # (/proc/PID/fd not available), so combine via temp files instead.
  printf '%s' "$runs"     > "$WORK/_runs.json"
  printf '%s' "$statuses" > "$WORK/_statuses.json"
  all=$(jq -s 'add // []' "$WORK/_runs.json" "$WORK/_statuses.json" 2>/dev/null || echo '[]')
  [ "$(echo "$all" | jq 'length')" = "0" ] && { echo -e "NO_CI\t"; return; }

  # Dependabot updater check-runs are named exactly "Dependabot".
  dep_fail=$(echo "$all" | jq '[.[]|select(.n=="Dependabot" and .c=="failure")]|length')
  app_fail=$(echo "$all" | jq '[.[]|select(.n!="Dependabot" and .c=="failure")]|length')
  app_other=$(echo "$all" | jq '[.[]|select(.n!="Dependabot" and (.c=="cancelled" or .c=="pending" or .c=="queued" or .c=="in_progress"))]|length')

  local detail
  detail=$(echo "$all" | jq -r '[.[]|"\(.n)=\(.c)"]|join(", ")')

  if   [ "$app_fail"  -gt 0 ]; then echo -e "BUILD_FAILED\t$detail"
  elif [ "$app_other" -gt 0 ]; then echo -e "STUCK\t$detail"
  elif [ "$dep_fail"  -gt 0 ]; then echo -e "DEPENDABOT_JOB_FAILED\t$detail"
  else                              echo -e "GREEN\t$detail"
  fi
}

# ---------------------------------------------------------------------------
# 5. Sweep
# ---------------------------------------------------------------------------
printf 'repo\tarchived\tdependabot_prs\tother_prs\talerts\tci_state\tdetail\n'
N_WAIVED_HIT=0
N_WAIVED_CI=0
while IFS=$'\t' read -r name isfork isarch branch; do
  [ -z "$name" ] && continue
  IFS=$'\t' read -r alerts alert_detail <<< "$(alerts_for "$name" "$isarch")"
  IFS=$'\t' read -r ci ci_detail        <<< "$(ci_for "$name" "$branch")"
  dpr=$(dpr_count "$name"); opr=$(opr_count "$name")

  # Emit only rows that represent an ACTIONABLE finding.
  # UNREADABLE is deliberately NOT a per-repo finding -- it is the norm for
  # every archived repo and would otherwise flood the report. It is surfaced
  # as a count in the summary instead, so the blind spot stays visible.
  # DISABLED IS a finding: alerts were switched off on a live repo.
  # DISABLED_OK is NOT: the operator declared that state intentional. It is
  # still counted and named in the summary so the waiver never becomes invisible.
  [ "$alerts" = "DISABLED_OK" ] && N_WAIVED_HIT=$((N_WAIVED_HIT + 1))
  finding=0
  # An open Dependabot PR stays a finding even on a waived repo: it is directly
  # mergeable, and its existence contradicts the premise that Dependabot is off.
  [ "$dpr" != "0" ] && finding=1
  [ "$alerts" = "DISABLED" ] && finding=1
  [ "$alerts" = "ERROR" ] && finding=1
  case "$alerts" in ''|*[!0-9]*) ;; *) [ "$alerts" -gt 0 ] && finding=1 ;; esac

  # NO_COMMITS is not a finding: an empty repo has nothing to build or expose.
  # On a waived repo DEPENDABOT_JOB_FAILED is not a finding either -- the updater
  # is off by intent, so its leftover check-runs are declared noise and cannot
  # re-run. BUILD_FAILED and STUCK still count on a waived repo: those are the
  # project's own CI and third-party statuses, nothing to do with Dependabot.
  case "$ci" in
    GREEN|NO_CI|NO_COMMITS) ;;
    DEPENDABOT_JOB_FAILED)
      if is_waived "$name"; then N_WAIVED_CI=$((N_WAIVED_CI + 1)); else finding=1; fi ;;
    *) finding=1 ;;
  esac

  if [ "$finding" = "1" ]; then
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$name" "$isarch" "$dpr" "$opr" "$alerts" "$ci" "${alert_detail}${alert_detail:+ | }${ci_detail}"
  fi
done < "$WORK/scope.tsv"

# ---------------------------------------------------------------------------
# 6. Summary
# ---------------------------------------------------------------------------
TOTAL_DPR=$(awk -F'\t' -v re="$DBOT_RE" '$3 ~ re' "$WORK/prs.tsv" | wc -l | tr -d ' ')
TOTAL_OPR=$(awk -F'\t' -v re="$DBOT_RE" '$3 !~ re' "$WORK/prs.tsv" | wc -l | tr -d ' ')

if [ "$INCLUDE_FORKS" = "1" ]; then
  FORK_LINE="forks_included:     $N_FORKS"
else
  FORK_LINE="forks_excluded:     $N_FORKS"
fi

cat <<SUMMARY

=== SUMMARY ===
owner:              $OWNER
repos_audited:      $TOTAL  (active=$N_ACTIVE archived=$N_ARCH)
$FORK_LINE
open_dependabot_prs:$TOTAL_DPR
open_other_prs:     $TOTAL_OPR

dependabot_waived:  $N_WAIVED_HIT of $N_WAIVED_CONFIGURED configured
$(if [ "$N_WAIVED_HIT" -gt 0 ]; then
    echo "  These repos were NOT measured -- Dependabot is disabled by intent, so any"
    echo "  advisory they would report is unseen. Their own build failures, stuck"
    echo "  statuses and open Dependabot PRs were still audited:"
    while read -r w; do [ -n "$w" ] && echo "    - $w"; done < "$WORK/waived.txt"
    [ "$N_WAIVED_CI" -gt 0 ] && echo "  $N_WAIVED_CI of them carry leftover updater check-run failures (suppressed)."
  fi)

NOTE: alert state for the $N_ARCH archived repos is UNREADABLE, not zero.
GitHub returns 403 on the alerts endpoint for archived repos, so their
vulnerability exposure is UNKNOWN and cannot be reported as clean.

CI states: GREEN | NO_CI | NO_COMMITS | BUILD_FAILED | DEPENDABOT_JOB_FAILED | STUCK
  BUILD_FAILED          = the project's own build/test failed. Real.
  DEPENDABOT_JOB_FAILED = Dependabot's updater job failed, app CI is fine.
  STUCK                 = a check or third-party status never reached a
                          terminal state (common on archived repos).
  NO_COMMITS            = empty repo, nothing to judge.

alerts: a number | UNREADABLE (archived) | DISABLED (off) | DISABLED_OK (waived)
        | ERROR
  ERROR is also unknown, never clean -- re-run those repos before concluding.
  UNREADABLE, DISABLED and DISABLED_OK are all UNMEASURED. Only a number is a
  measurement. Never sum them into a single "0 advisories" claim.
SUMMARY
