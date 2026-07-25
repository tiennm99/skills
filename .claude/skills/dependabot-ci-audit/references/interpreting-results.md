# Interpreting results & remediation patterns

Load when findings need diagnosis or the user asks how to fix them.
This skill is read-only — describe fixes, let the user apply them.

## Why Dependabot opens no PR despite open alerts

Dependabot bumps the **vulnerable package within existing constraints**. It does
not walk up the tree to bump whatever parent pins that package. When no
in-constraint version satisfies the advisory, it reports one of:

| Error | Meaning |
|---|---|
| `security_update_not_possible` | The fixed version is unreachable in this tree |
| `update_not_possible` | Same, for a non-security update |

The log line carries the numbers:

```
security_update_not_possible | { "dependency-name": "picomatch",
  "latest-resolvable-version": "2.3.1",
  "lowest-non-vulnerable-version": "2.3.2" }
```

### First: does the fixed version even exist?

```bash
npm view <pkg> versions --json | tr -d '[]" ' | tr ',' '\n' | tail -10
npm view <pkg> version
npm view <pkg>@<fixed> engines --json     # may require a newer runtime
```

If it was never published, nothing can fix the advisory and the finding should
be reported as permanently blocked. If it exists, a parent constraint is the
blocker and the pattern below usually applies.

## The grandparent-bump pattern

Most `security_update_not_possible` cases resolve by upgrading the ancestor that
pins the vulnerable package, because a newer ancestor already depends on a
patched version.

Worked example:

```
wrangler 4.106.0 → miniflare 4.20260630.0 → sharp 0.34.5   (vulnerable)
wrangler 4.114.0 → miniflare 4.20260722.0 → sharp 0.35.2   (patched)
```

Find the ancestor and check whether a current release already carries the fix:

```bash
npm view <ancestor> version
npm view <ancestor> dependencies --json
npm view <intermediate> dependencies --json | grep -i <vulnerable-pkg>
```

If the declared range already permits the newer ancestor (e.g. `^4.106.0` allows
`4.114.0`), only the lockfile is holding it back — a lockfile refresh suffices
and no source change is needed.

## The verification trap — this one matters

**Check that the vulnerable version is ABSENT. Do not check that the fixed
version is PRESENT.**

Bumping one ancestor can introduce a second resolution path while the vulnerable
copy survives via a *different* exact-pinned parent. The lockfile then contains
both, and a presence check passes while the advisory stays open.

Real case: bumping top-level `wrangler` pulled `sharp@0.35.2`, but
`@cloudflare/vitest-pool-workers@0.18.6` exact-pinned `miniflare@4.20260714.0`
→ `sharp@0.34.5`, still live. Fixing it required bumping that package too.

```bash
# WRONG — passes while still vulnerable
grep -q "sharp@0.35" pnpm-lock.yaml && echo fixed

# RIGHT — the vulnerable version must be gone
grep -oE "sharp@[0-9.]+" pnpm-lock.yaml | sort -u   # expect no 0.34.x
```

So: enumerate **every** parent pinning the package, not just the obvious one.

## Cases that are genuinely unfixable

| Situation | Why | Recommendation |
|---|---|---|
| Lockfile with no manifest (`package-lock.json`, no `package.json`) | The npm updater needs a manifest; it can never run | Reconstruct a manifest only if the project is live. On a retired repo, skip |
| Two incompatible major branches of one package required by different parents | An override breaks one consumer | Upgrade or drop the parent that pins the old branch |
| Advisory in a deprecated package (e.g. `request`) | No patched release will ever ship | Replace the library, or accept on a retired project |
| Third-party commit status stuck `pending` (Vercel, etc.) | The integration owns the status; often orphaned (`creator: null`) | Not fixable from our side; a later commit may clear it |

## Archived repositories

- Actions do not run, so CI is **frozen** at the last commit before archiving.
- Alerts are unreadable (403). Exposure is unknown, not zero.
- A `Dependabot=failure` check-run can still be bound to HEAD and cannot be
  cleared without a new commit.
- **Unarchiving to fix has a measured cost:** each unarchive re-runs failing
  updater jobs, and each failure binds permanently to the then-current HEAD. In
  one observed campaign the badge got worse while security improved — one repo
  went from 6 to 12 failing checks with zero advisories resolved. Weigh whether
  a cosmetic badge on a retired project justifies it.
- If a repo is unarchived to apply a fix, re-archive only after **no run is
  `in_progress` or `queued`**, polling for several consecutive clear reads.
  Re-archiving mid-run strands runs permanently `queued` and makes
  `actions/deploy-pages` fail with `HttpError: Repository is archived` (409).

## Repos where alerts are disabled

Switched off means nothing is detected. On an active repo this is a real gap:
Dependabot may still deliver *version* updates while *security* alerts are off,
so the repo looks maintained yet its exposure is unmeasured. Recommend enabling
alerts; do not enable them as part of a read-only audit.

## Useful queries

```bash
# every open PR for an owner, one call
gh search prs --owner OWNER --state open --json repository,number,title,author

# alerts with severity + fixed version (non-archived only)
gh api "repos/OWNER/REPO/dependabot/alerts?state=open&per_page=100" \
  --jq '.[]|"\(.security_advisory.severity) \(.dependency.package.name) fix=\(.security_vulnerability.first_patched_version.identifier // "NONE")"'

# HEAD commit: check-runs and commit statuses
SHA=$(gh api repos/OWNER/REPO/commits/$(gh api repos/OWNER/REPO --jq .default_branch) --jq .sha)
gh api repos/OWNER/REPO/commits/$SHA/check-runs --jq '.check_runs[]|"\(.name)=\(.conclusion)"'
gh api repos/OWNER/REPO/commits/$SHA/status      --jq '.statuses[]|"\(.context)=\(.state)"'

# repo-level toggles
gh api repos/OWNER/REPO/automated-security-fixes    # security-update PRs
gh api repos/OWNER/REPO/vulnerability-alerts -i     # 204 = enabled
```

Note `automated-security-fixes` is a **repo setting**, independent of
`.github/dependabot.yml`. Deleting that file stops routine version updates but
does **not** stop security-update runs.
