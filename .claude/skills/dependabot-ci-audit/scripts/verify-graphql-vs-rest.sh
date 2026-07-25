#!/usr/bin/env bash
# Regression gate: prove the batched GraphQL CI sweep classifies identically to
# the per-repo REST path before GraphQL becomes the default.
#
# This is not ceremony. The only reason the statusCheckRollup blind spot was ever
# found is that these two paths disagreed on the same commit. Re-run this gate
# after ANY edit to ci-sweep.graphql or classify-ci.jq.
#
# Usage:
#   ./verify-graphql-vs-rest.sh [owner] [repo_limit]      # default limit 50
#
# Exit 0 only when every repo in the slice classifies the same on both paths.

set -uo pipefail

OWNER="${1:-$(gh api user --jq .login)}"
LIMIT="${2:-50}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Repos that exercise the cases most likely to diverge. Asserted present in the
# slice rather than assumed: REPO_LIMIT slices by PUSH DATE, which reorders as
# repos are pushed to. These moved 46/47 -> 51/52 within a single session, so a
# hardcoded limit silently stops covering them.
INTERESTING="${INTERESTING:-claudekit-engineer claudekit-marketing chambai exchange-rate-export}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# gh emits CRLF on Windows; without stripping it every value mismatches and the
# diff is meaningless. LC_ALL=C is pinned on sort AND diff so collation agrees.
# EMIT_ALL=1 is essential: comparing only FINDINGS rows can diff two empty sets
# and report PASS while proving nothing. Every repo in the slice must be compared,
# archived and clean ones included.
run_path() {
  local src="$1" out="$2"
  EMIT_ALL=1 CI_SOURCE="$src" REPO_LIMIT="$LIMIT" bash "$SCRIPT_DIR/audit-repos.sh" "$OWNER" \
    > "$WORK/raw-$src.txt" 2>"$WORK/err-$src.txt" || {
      echo "ERROR: $src path exited non-zero" >&2; sed 's/^/  /' "$WORK/err-$src.txt" >&2; return 1; }
  # Keyed repo -> alerts / ci_state. Filter by CONTENT, not line number: EMIT_ALL
  # output has no header row, so an NR>1 guard silently drops a real repo.
  awk -F'\t' 'NF>=6 && $1!="repo" && $0 !~ /^=== / {print $1"\t"$5"\t"$6}' "$WORK/raw-$src.txt" \
    | sed '/^$/d' | tr -d '\r' | LC_ALL=C sort > "$out"
}

echo "== gate: $OWNER, slice=$LIMIT =="

echo "-- REST path --"
t0=$SECONDS
run_path rest "$WORK/rest.tsv" || exit 1
REST_SECS=$((SECONDS - t0))

echo "-- GraphQL path --"
t0=$SECONDS
run_path graphql "$WORK/gql.tsv" || exit 1
GQL_SECS=$((SECONDS - t0))

# The slice must actually contain the interesting repos, or a clean diff proves
# nothing. Derived from the REST run's own inventory, not guessed.
gh repo list "$OWNER" --limit "$LIMIT" --json name,isFork \
  --jq '.[]|select(.isFork|not)|.name' | tr -d '\r' | LC_ALL=C sort > "$WORK/slice.txt"
missing=""
for r in $INTERESTING; do
  grep -qxF "$r" "$WORK/slice.txt" || missing="$missing $r"
done
if [ -n "$missing" ]; then
  echo "FAIL: slice of $LIMIT does not cover:$missing" >&2
  echo "      raise the limit until it does -- a clean diff over the wrong repos proves nothing." >&2
  exit 1
fi
echo "-- slice covers all interesting repos --"

ROWS=$(wc -l < "$WORK/rest.tsv" | tr -d ' ')

# Compare against the number of repos actually AUDITED, not REPO_LIMIT. The limit
# counts raw repos before forks are filtered out, so a fork entering the slice
# makes scope smaller than the limit -- which is correct, not a failure.
# Default whitespace splitting: $2 is the count alone. Do NOT strip non-digits
# from the whole field -- that concatenates the "(active=N archived=M)" numbers.
AUDITED=$(awk '/^repos_audited:/{print $2; exit}' "$WORK/raw-rest.txt")
case "$AUDITED" in ''|*[!0-9]*) echo "FAIL: could not read repos_audited from the REST run." >&2; exit 1 ;; esac

# A diff of two empty (or partial) sets "passes" while proving nothing. Refuse it.
if [ "$ROWS" -eq 0 ] || [ "$ROWS" -ne "$AUDITED" ]; then
  echo "FAIL: compared $ROWS rows but $AUDITED repos were audited -- coverage is incomplete," >&2
  echo "      so a clean diff would be vacuous. Check EMIT_ALL wiring." >&2
  exit 1
fi

# Classification must be exercised, not just uniform. All-NO_CI would pass trivially.
DISTINCT=$(cut -f3 "$WORK/rest.tsv" | LC_ALL=C sort -u | tr '\n' ' ')
echo "-- ci_states exercised: $DISTINCT"

echo "-- diff (repo / alerts / ci_state), $ROWS of $AUDITED audited --"
if LC_ALL=C diff -u "$WORK/rest.tsv" "$WORK/gql.tsv"; then
  echo
  echo "PASS: identical classification on both paths across $ROWS repos."
  echo "  rest=${REST_SECS}s  graphql=${GQL_SECS}s  speedup=$(( REST_SECS / (GQL_SECS>0?GQL_SECS:1) ))x"
  exit 0
fi
echo
echo "FAIL: paths disagree. Do NOT make GraphQL the default until this is empty." >&2
exit 1
