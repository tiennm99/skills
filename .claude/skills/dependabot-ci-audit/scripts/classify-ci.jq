# Classify each repo's CI state from the batched sweep in ci-sweep.graphql.
#
# Input:  the concatenated pages of `gh api graphql --paginate` (use jq -s).
# Output: one TSV row per repo -- name, archived, ci_state, dep_fail, app_fail,
#         truncated, detail
#
# Mirrors ci_for() in audit-repos.sh exactly. Any divergence here is a silent
# wrong answer, which is why the two paths are diffed before this one becomes
# the default.

def contexts:
  ( (.status.contexts // []) | map({n: .context, c: .state}) )
  + ( [ .checkSuites.nodes[]? | .checkRuns.nodes[]? | {n: .name, c: (.conclusion // .status)} ] )
  | map({ n: (.n // "?"), c: ((.c // "unknown") | ascii_downcase) });

# A page that came back short is an UNKNOWN, not a measurement. Callers must
# treat this as fatal rather than reporting the partial state as clean.
def truncated:
  ( (.checkSuites.totalCount // 0) > ((.checkSuites.nodes // []) | length) )
  or ( [ .checkSuites.nodes[]?
         | select((.checkRuns.totalCount // 0) > ((.checkRuns.nodes // []) | length)) ]
       | length > 0 );

# Terminal-failure and not-yet-terminal state vocabularies, kept in one place.
def failed($c):   ["failure","error","timed_out","startup_failure","action_required"] | index($c) != null;
def unsettled($c): ["pending","queued","in_progress","waiting","requested","expected","cancelled"] | index($c) != null;

[ .[] | .data.repositoryOwner.repositories.nodes[] ]
| unique_by(.name)
| .[]
| . as $r
| ($r.defaultBranchRef.target // null) as $t
| (if $t == null then [] else ($t | contexts) end) as $ctx
# The updater check-run is named EXACTLY "Dependabot". Do not broaden to
# "Dependabot / *": that would swallow a user workflow named Dependabot and
# understate real breakage -- an error in the dangerous direction.
| ($ctx | map(select(.n != "Dependabot"))) as $app
| ($ctx | map(select(.n == "Dependabot"))) as $dep
| ($app | map(select(failed(.c)))    | length) as $app_fail
| ($app | map(select(unsettled(.c))) | length) as $app_other
| ($dep | map(select(failed(.c)))    | length) as $dep_fail
| [
    $r.name,
    ($r.isArchived | tostring),
    ( if $r.defaultBranchRef == null then "NO_COMMITS"
      elif ($ctx | length) == 0      then "NO_CI"
      elif $app_fail  > 0            then "BUILD_FAILED"
      elif $app_other > 0            then "STUCK"
      elif $dep_fail  > 0            then "DEPENDABOT_JOB_FAILED"
      else "GREEN" end ),
    ($dep_fail | tostring),
    ($app_fail | tostring),
    (if $t == null then "false" else ($t | truncated | tostring) end),
    ($ctx | map("\(.n)=\(.c)") | join(", "))
  ]
| @tsv
