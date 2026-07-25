#!/usr/bin/env bash
# Account-wide Dependabot + GitHub Actions CI audit. READ-ONLY.
#
# Emits ACTIONABLE / FROZEN / WAIVED tiers, then a summary block. Exit status and
# the finding count reflect ACTIONABLE only.
# Never writes: no merges, no commits, no archive toggling, no run deletion.
#
# Usage:
#   ./audit-repos.sh [owner]                     # defaults to authenticated user
#   REPO_LIMIT=50 ./audit-repos.sh owner         # sample slice (raw, pre-fork-filter)
#   CI_SOURCE=rest ./audit-repos.sh owner        # old per-repo path, second opinion
#   VERIFY_REPO=name ./audit-repos.sh owner      # REST spot-check of one repo
#   INCLUDE_FORKS=1 CI_SOURCE=rest ./audit-repos.sh owner   # forks need REST
#   EMIT_ALL=1 ./audit-repos.sh owner            # flat, every repo (for diffing)
#
# Forks are excluded by default. The batched sweep filters isFork:false, so
# auditing forks requires CI_SOURCE=rest.
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
# graphql = one batched sweep for every repo (see ci-sweep.graphql). Default.
# rest    = 3 REST calls per repo (~4.3s/repo). Kept as an independent second
#           opinion: a REST/GraphQL disagreement is what exposed the
#           statusCheckRollup blind spot. Re-verify with VERIFY_REPO or
#           scripts/verify-graphql-vs-rest.sh after touching the query or jq.
CI_SOURCE_EXPLICIT="${CI_SOURCE:+1}"
CI_SOURCE="${CI_SOURCE:-graphql}"

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

# Lookups are associative arrays, not per-repo grep/awk calls. On Windows Git
# Bash a process spawn costs ~80ms, so 4 forks x 221 repos added ~70s of pure
# overhead -- more than every API call combined. Populate once, read in-process.
declare -A WAIVED_M
while read -r w; do [ -n "$w" ] && WAIVED_M["$w"]=1; done < "$WORK/waived.txt"
is_waived() { [ -n "${WAIVED_M[$1]:-}" ]; }

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
# One awk pass tallies every repo, instead of two awk calls per repo.
# Via a temp file, NOT process substitution: <(...) is unreliable on Windows Git
# Bash because /proc/PID/fd is missing.
awk -F'\t' -v re="$DBOT_RE" '
  { if ($3 ~ re) d[$1]++; else o[$1]++ }
  END { for (r in d) printf "D\t%s\t%s\n", r, d[r]
        for (r in o) printf "O\t%s\t%s\n", r, o[r] }' "$WORK/prs.tsv" > "$WORK/prcounts.tsv"
declare -A DPR_N OPR_N
while IFS=$'\t' read -r kind repo n; do
  case "$kind" in D) DPR_N["$repo"]=$n ;; O) OPR_N["$repo"]=$n ;; esac
done < "$WORK/prcounts.tsv"

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
# 4b. Batched CI sweep (CI_SOURCE=graphql).
#     One paginated query replaces 3 REST calls per repo. Every failure mode
#     below is FATAL on purpose: a sweep that returns nothing would otherwise
#     classify every repo as NO_CI, i.e. render "unknown" as "all green" --
#     the single most damaging error this audit can make.
# ---------------------------------------------------------------------------
graphql_sweep() {
  local raw="$WORK/ci-raw.json" expected got foreign trunc
  gh api graphql --paginate -f owner="$OWNER" \
      -F query=@"$SCRIPT_DIR/ci-sweep.graphql" > "$raw" 2>"$WORK/ci-err.txt" || {
    echo "ERROR: GraphQL CI sweep failed (exit $?). Not reporting partial results." >&2
    sed 's/^/  gh: /' "$WORK/ci-err.txt" >&2
    exit 1
  }
  [ -s "$raw" ] || { echo "ERROR: GraphQL CI sweep returned no data." >&2; exit 1; }

  expected=$(jq -sr '.[0].data.repositoryOwner.repositories.totalCount // "x"' "$raw")
  got=$(jq -sr '[.[]|.data.repositoryOwner.repositories.nodes[].name]|unique|length' "$raw")
  case "$expected" in ''|*[!0-9]*) echo "ERROR: sweep returned no totalCount." >&2; exit 1 ;; esac
  [ "$got" = "$expected" ] || {
    echo "ERROR: sweep incomplete -- totalCount=$expected but $got unique repos returned." >&2
    exit 1
  }

  # Repos owned by another account mean the affiliation pins were lost.
  foreign=$(jq -sr --arg o "$OWNER/" \
    '[.[]|.data.repositoryOwner.repositories.nodes[]|select(.nameWithOwner|startswith($o)|not)]|length' "$raw")
  [ "$foreign" = "0" ] || {
    echo "ERROR: sweep returned $foreign repos not owned by $OWNER (affiliation pins lost)." >&2
    exit 1
  }

  jq -sr -f "$SCRIPT_DIR/classify-ci.jq" "$raw" > "$WORK/ci.tsv" || {
    echo "ERROR: CI classification failed." >&2; exit 1; }

  # A short page is an unknown, not a measurement.
  trunc=$(awk -F'\t' '$6=="true"{print $1}' "$WORK/ci.tsv")
  if [ -n "$trunc" ]; then
    echo "ERROR: check data truncated for these repos; raise page size before trusting results:" >&2
    printf '  %s\n' $trunc >&2
    exit 1
  fi
}

# Loaded into memory once. A repo absent from the sweep is ERROR (unknown), never
# a silent GREEN.
declare -A CI_ST CI_DT
load_sweep_lookup() {
  local n a st df af tr dt
  while IFS=$'\t' read -r n a st df af tr dt; do
    [ -n "$n" ] && { CI_ST["$n"]="$st"; CI_DT["$n"]="$dt"; }
  done < "$WORK/ci.tsv"
}

[ "$CI_SOURCE" = "rest" ] || [ "$CI_SOURCE" = "graphql" ] || {
  echo "ERROR: CI_SOURCE must be 'rest' or 'graphql' (got '$CI_SOURCE')" >&2; exit 1; }

# VERIFY_REPO: audit ONE repo through the REST path and print its classification.
# The independent second opinion that catches GraphQL-side blind spots.
if [ -n "${VERIFY_REPO:-}" ]; then
  if [ "$CI_SOURCE_EXPLICIT" = "1" ] && [ "$CI_SOURCE" = "graphql" ]; then
    echo "ERROR: VERIFY_REPO is the REST spot-check; it cannot be combined with CI_SOURCE=graphql." >&2
    exit 1
  fi
  v_arch=$(gh api "repos/$OWNER/$VERIFY_REPO" --jq '.archived|tostring' 2>/dev/null) \
    || { echo "ERROR: cannot read repos/$OWNER/$VERIFY_REPO" >&2; exit 1; }
  v_br=$(gh api "repos/$OWNER/$VERIFY_REPO" --jq '.default_branch // ""' 2>/dev/null)
  IFS=$'\t' read -r v_alerts v_adet <<< "$(alerts_for "$VERIFY_REPO" "$v_arch")"
  IFS=$'\t' read -r v_ci v_cdet     <<< "$(ci_for "$VERIFY_REPO" "$v_br")"
  printf 'repo\tarchived\talerts\tci_state\tdetail\n'
  printf '%s\t%s\t%s\t%s\t%s\n' "$VERIFY_REPO" "$v_arch" "$v_alerts" "$v_ci" \
    "${v_adet}${v_adet:+ | }${v_cdet}"
  echo "(REST spot-check via VERIFY_REPO; compare against the default GraphQL run)"
  exit 0
fi

# The batched sweep hardcodes isFork:false, so it cannot supply CI state for
# forks. Fail loudly rather than reporting every fork as ERROR.
if [ "$CI_SOURCE" = "graphql" ] && [ "$INCLUDE_FORKS" = "1" ]; then
  echo "ERROR: INCLUDE_FORKS=1 needs CI_SOURCE=rest -- the batched sweep excludes forks." >&2
  exit 1
fi

if [ "$CI_SOURCE" = "graphql" ]; then
  graphql_sweep
  load_sweep_lookup
fi

# ---------------------------------------------------------------------------
# 5. Sweep
# ---------------------------------------------------------------------------
#    Rows are buffered into three tiers instead of one flat list:
#      ACTIONABLE - active repos, things that can be acted on right now
#      FROZEN     - archived repos. Nothing on them is actionable without
#                   unarchiving: alerts 403, PRs unmergeable (read-only repo),
#                   Actions frozen. Informational, never a finding -- but red
#                   ones are still NAMED, because an archived finding is worth
#                   knowing even when acting on it needs a state change first.
#      WAIVED     - Dependabot disabled by intent (config file)
#    Exit status and the finding count reflect ACTIONABLE only.
N_WAIVED_HIT=0
N_WAIVED_CI=0
: > "$WORK/tier-actionable.tsv"
: > "$WORK/tier-frozen.tsv"
N_FROZEN_DPR=0
while IFS=$'\t' read -r name isfork isarch branch; do
  [ -z "$name" ] && continue
  IFS=$'\t' read -r alerts alert_detail <<< "$(alerts_for "$name" "$isarch")"
  if [ "$CI_SOURCE" = "graphql" ]; then
    ci="${CI_ST[$name]:-ERROR}"; ci_detail="${CI_DT[$name]:-}"
  else
    IFS=$'\t' read -r ci ci_detail      <<< "$(ci_for "$name" "$branch")"
  fi
  dpr="${DPR_N[$name]:-0}"; opr="${OPR_N[$name]:-0}"

  row=$(printf '%s\t%s\t%s\t%s\t%s\t%s\t%s' \
    "$name" "$isarch" "$dpr" "$opr" "$alerts" "$ci" "${alert_detail}${alert_detail:+ | }${ci_detail}")

  # EMIT_ALL=1 prints every repo flat, bypassing tiers. Used by
  # verify-graphql-vs-rest.sh: comparing only findings can diff two empty sets
  # and "pass" while proving nothing about classification.
  if [ "${EMIT_ALL:-0}" = "1" ]; then printf '%s\n' "$row"; continue; fi

  # --- FROZEN: archived. Nothing here is actionable without unarchiving. -----
  if [ "$isarch" = "true" ]; then
    [ "$dpr" != "0" ] && N_FROZEN_DPR=$((N_FROZEN_DPR + dpr))
    case "$ci" in
      BUILD_FAILED|STUCK|DEPENDABOT_JOB_FAILED) printf '%s\n' "$row" >> "$WORK/tier-frozen.tsv" ;;
    esac
    continue
  fi

  # --- WAIVED: Dependabot off by intent. Counted and named, never a finding. -
  [ "$alerts" = "DISABLED_OK" ] && N_WAIVED_HIT=$((N_WAIVED_HIT + 1))

  # --- ACTIONABLE: active repos only. ---------------------------------------
  # DISABLED IS a finding: alerts switched off on a live repo is real exposure.
  # DISABLED_OK is not -- the operator declared that state intentional.
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

  [ "$finding" = "1" ] && printf '%s\n' "$row" >> "$WORK/tier-actionable.tsv"
done < "$WORK/scope.tsv"

# ---------------------------------------------------------------------------
# 5b. Tiered output. EMIT_ALL already printed a flat list and skipped this.
# ---------------------------------------------------------------------------
if [ "${EMIT_ALL:-0}" != "1" ]; then
  N_ACTIONABLE=$(wc -l < "$WORK/tier-actionable.tsv" | tr -d ' ')
  N_FROZEN_RED=$(wc -l < "$WORK/tier-frozen.tsv" | tr -d ' ')
  HDR='repo\tarchived\tdependabot_prs\tother_prs\talerts\tci_state\tdetail'

  echo "=== ACTIONABLE ($N_ACTIONABLE) — active repos, act now ==="
  if [ "$N_ACTIONABLE" = "0" ]; then
    echo "(none)"          # explicit: measured none, not a missing section
  else
    printf "$HDR\n"; cat "$WORK/tier-actionable.tsv"
  fi

  echo
  echo "=== FROZEN ($N_ARCH archived) — needs unarchiving before anything is actionable ==="
  echo "alert state: UNREADABLE on all $N_ARCH (403). UNKNOWN, not zero."
  echo "open Dependabot PRs on archived repos: $N_FROZEN_DPR (unmergeable while archived)"
  if [ "$N_FROZEN_RED" = "0" ]; then
    echo "non-green CI: (none)"
  else
    echo "non-green CI ($N_FROZEN_RED), listed so they stay discoverable:"
    printf "$HDR\n"; cat "$WORK/tier-frozen.tsv"
  fi
fi

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

=== WAIVED ($N_WAIVED_HIT of $N_WAIVED_CONFIGURED configured) ===
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
