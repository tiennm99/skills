package audit

import (
	"fmt"
	"io"
)

const rowHeader = "repo\tarchived\tdependabot_prs\tother_prs\talerts\tci_state\tdetail"

// Tiers splits the measured rows by what can actually be done about them.
type Tiers struct {
	// Actionable is active repos only. The finding count and any exit status
	// reflect this tier alone.
	Actionable []Row
	// Frozen is archived repos with non-green CI. Never a finding -- alerts 403,
	// PRs are unmergeable on a read-only repo and Actions are frozen, so nothing
	// is actionable until someone unarchives, which this tool does not do. They
	// are still NAMED, because an archived failure is worth knowing about even
	// when acting on it needs a state change first.
	Frozen []Row
	// FrozenDependabotPRs counts open Dependabot PRs stranded on archived repos.
	FrozenDependabotPRs int
	// WaivedHit is waived repos that were actually in scope, which is not the
	// number configured: a waiver for a deleted repo matches nothing.
	WaivedHit int
	// WaivedCI is waived repos carrying suppressed updater check-run failures.
	WaivedCI int
}

// Tiers computes the three tiers.
func (res *Result) Tiers() Tiers {
	var t Tiers
	for _, row := range res.Rows {
		if row.Archived {
			t.FrozenDependabotPRs += row.DependabotPRs
			switch row.CI {
			case CIBuildFailed, CIStuck, CIDependabotJob:
				t.Frozen = append(t.Frozen, row)
			}
			continue
		}

		waived := res.Waivers.Has(row.Name)
		if row.Alerts.Label == AlertsDisabledOK {
			t.WaivedHit++
		}
		if waived && row.CI == CIDependabotJob {
			t.WaivedCI++
		}
		if row.IsFinding(waived) {
			t.Actionable = append(t.Actionable, row)
		}
	}
	return t
}

// WriteReport prints the tiered report followed by the summary.
func WriteReport(w io.Writer, res *Result) {
	t := res.Tiers()
	archived := res.Archived()

	fmt.Fprintf(w, "=== ACTIONABLE (%d) — active repos, act now ===\n", len(t.Actionable))
	if len(t.Actionable) == 0 {
		// Explicit, so an empty tier reads as MEASURED none rather than as a
		// section that failed to render.
		fmt.Fprintln(w, "(none)")
	} else {
		writeRows(w, t.Actionable)
	}

	fmt.Fprintf(w, "\n=== FROZEN (%d archived) — needs unarchiving before anything is actionable ===\n", archived)
	fmt.Fprintf(w, "alert state: UNREADABLE on all %d (403). UNKNOWN, not zero.\n", archived)
	fmt.Fprintf(w, "open Dependabot PRs on archived repos: %d (unmergeable while archived)\n", t.FrozenDependabotPRs)
	if len(t.Frozen) == 0 {
		fmt.Fprintln(w, "non-green CI: (none)")
	} else {
		fmt.Fprintf(w, "non-green CI (%d), listed so they stay discoverable:\n", len(t.Frozen))
		writeRows(w, t.Frozen)
	}

	writeSummary(w, res, t)
}

// WriteFlat prints every audited repo as one row and skips the tiers, for
// diffing two runs against each other. Findings-only output can diff two empty
// sets and "match" while proving nothing about classification.
func WriteFlat(w io.Writer, res *Result) {
	for _, row := range res.Rows {
		fmt.Fprintln(w, row.TSV())
	}
	// Tier counts are deliberately zeroed here: this mode reports no tiers, so
	// claiming waived hits it never grouped would be inventing a measurement.
	writeSummary(w, res, Tiers{})
}

func writeRows(w io.Writer, rows []Row) {
	fmt.Fprintln(w, rowHeader)
	for _, row := range rows {
		fmt.Fprintln(w, row.TSV())
	}
}

func writeSummary(w io.Writer, res *Result, t Tiers) {
	archived := res.Archived()
	forkLabel := "forks_excluded:"
	if res.IncludeForks {
		forkLabel = "forks_included:"
	}

	fmt.Fprintf(w, "\n=== SUMMARY ===\n")
	fmt.Fprintf(w, "owner:              %s\n", res.Owner)
	fmt.Fprintf(w, "repos_audited:      %d  (active=%d archived=%d)\n", len(res.Rows), res.Active(), archived)
	fmt.Fprintf(w, "%-20s%d\n", forkLabel, res.Forks)
	fmt.Fprintf(w, "open_dependabot_prs:%d\n", res.TotalDependabotPRs)
	fmt.Fprintf(w, "open_other_prs:     %d\n", res.TotalOtherPRs)

	fmt.Fprintf(w, "\n=== WAIVED (%d of %d configured) ===\n", t.WaivedHit, res.Waivers.Len())
	if t.WaivedHit > 0 {
		fmt.Fprintln(w, "  These repos were NOT measured -- Dependabot is disabled by intent, so any")
		fmt.Fprintln(w, "  advisory they would report is unseen. Their own build failures, stuck")
		fmt.Fprintln(w, "  statuses and open Dependabot PRs were still audited:")
		for _, name := range res.Waivers.Names() {
			fmt.Fprintf(w, "    - %s\n", name)
		}
		if t.WaivedCI > 0 {
			fmt.Fprintf(w, "  %d of them carry leftover updater check-run failures (suppressed).\n", t.WaivedCI)
		}
	}

	fmt.Fprintf(w, `
NOTE: alert state for the %d archived repos is UNREADABLE, not zero.
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
`, archived)
}

// WriteSpotCheck prints one repo audited through the REST path, in the narrower
// column set a single-repo check needs.
func WriteSpotCheck(w io.Writer, row Row) {
	fmt.Fprintln(w, "repo\tarchived\talerts\tci_state\tdetail")
	fmt.Fprintf(w, "%s\t%t\t%s\t%s\t%s\n", row.Name, row.Archived, row.Alerts, row.CI, row.Detail())
	fmt.Fprintln(w, "(REST spot-check; compare against the default GraphQL run)")
}
