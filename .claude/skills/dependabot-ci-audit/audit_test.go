package audit

import (
	"strings"
	"testing"
)

func TestIsFinding(t *testing.T) {
	tests := []struct {
		name   string
		row    Row
		waived bool
		want   bool
	}{
		{
			name: "clean active repo",
			row:  Row{Alerts: AlertState{Count: 0}, CI: CIGreen},
		},
		{
			name: "open alerts",
			row:  Row{Alerts: AlertState{Count: 3}, CI: CIGreen},
			want: true,
		},
		{
			// Alerts switched off on a live repo is real, unmeasured exposure.
			name: "alerts disabled on an active repo",
			row:  Row{Alerts: AlertState{Label: AlertsDisabled}, CI: CIGreen},
			want: true,
		},
		{
			// ERROR is unknown, and unknown is never clean.
			name: "unreadable alerts",
			row:  Row{Alerts: AlertState{Label: AlertsError}, CI: CIGreen},
			want: true,
		},
		{
			name: "build failed",
			row:  Row{Alerts: AlertState{Count: 0}, CI: CIBuildFailed},
			want: true,
		},
		{
			name: "empty repo has nothing to expose",
			row:  Row{Alerts: AlertState{Count: 0}, CI: CINoCommits},
		},
		{
			name: "no CI configured is not a finding",
			row:  Row{Alerts: AlertState{Count: 0}, CI: CINoCI},
		},
		{
			name: "updater failure on an unwaived repo",
			row:  Row{Alerts: AlertState{Count: 0}, CI: CIDependabotJob},
			want: true,
		},
		{
			// With the updater off by intent, its leftover check-runs cannot
			// re-run, so they are noise.
			name:   "updater failure on a waived repo is suppressed",
			row:    Row{Alerts: AlertState{Label: AlertsDisabledOK}, CI: CIDependabotJob},
			waived: true,
		},
		{
			// The three things a waiver must NOT hide.
			name:   "a waived repo's own build failure still counts",
			row:    Row{Alerts: AlertState{Label: AlertsDisabledOK}, CI: CIBuildFailed},
			waived: true,
			want:   true,
		},
		{
			name:   "a waived repo's stuck status still counts",
			row:    Row{Alerts: AlertState{Label: AlertsDisabledOK}, CI: CIStuck},
			waived: true,
			want:   true,
		},
		{
			name:   "an open Dependabot PR still counts on a waived repo",
			row:    Row{DependabotPRs: 1, Alerts: AlertState{Label: AlertsDisabledOK}, CI: CIGreen},
			waived: true,
			want:   true,
		},
		{
			name: "human PRs alone are not a finding",
			row:  Row{OtherPRs: 4, Alerts: AlertState{Count: 0}, CI: CIGreen},
		},
		{
			// DISABLED_OK is an accepted blind spot, not a measured zero -- but
			// not a finding either.
			name:   "waived and otherwise clean",
			row:    Row{Alerts: AlertState{Label: AlertsDisabledOK}, CI: CIGreen},
			waived: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.IsFinding(tc.waived); got != tc.want {
				t.Errorf("IsFinding(waived=%v) = %v, want %v", tc.waived, got, tc.want)
			}
		})
	}
}

// Archived repos are informational: nothing on them can be acted on without
// unarchiving, which this tool does not do. But a red one must still be NAMED.
func TestTiersSeparatesArchivedFromActionable(t *testing.T) {
	res := &Result{
		Waivers: ParseWaivers("waived-repo\n"),
		Rows: []Row{
			{Name: "broken", Alerts: AlertState{Count: 0}, CI: CIBuildFailed},
			{Name: "frozen-broken", Archived: true, Alerts: AlertState{Label: AlertsUnreadable}, CI: CIBuildFailed},
			{Name: "frozen-green", Archived: true, Alerts: AlertState{Label: AlertsUnreadable}, CI: CIGreen},
			{Name: "frozen-with-prs", Archived: true, DependabotPRs: 2, Alerts: AlertState{Label: AlertsUnreadable}, CI: CIGreen},
			{Name: "waived-repo", Alerts: AlertState{Label: AlertsDisabledOK}, CI: CIDependabotJob},
		},
	}

	tiers := res.Tiers()

	if len(tiers.Actionable) != 1 || tiers.Actionable[0].Name != "broken" {
		t.Errorf("actionable = %v, want just [broken]", rowNames(tiers.Actionable))
	}
	if len(tiers.Frozen) != 1 || tiers.Frozen[0].Name != "frozen-broken" {
		t.Errorf("frozen = %v, want just [frozen-broken]", rowNames(tiers.Frozen))
	}
	if tiers.FrozenDependabotPRs != 2 {
		t.Errorf("stranded archived Dependabot PRs = %d, want 2", tiers.FrozenDependabotPRs)
	}
	if tiers.WaivedHit != 1 || tiers.WaivedCI != 1 {
		t.Errorf("waived hit/ci = %d/%d, want 1/1", tiers.WaivedHit, tiers.WaivedCI)
	}
}

// Skipping archived repos removes them from the tiers. It must NOT remove them
// from the disclosure, or an account that is mostly archived reads as fully
// audited and clean.
func TestWriteReportDisclosesSkippedArchived(t *testing.T) {
	res := &Result{
		Owner:                "someone",
		Waivers:              ParseWaivers(""),
		Archived:             195,
		SkippedDependabotPRs: 3,
		Rows: []Row{
			{Name: "clean", Alerts: AlertState{Count: 0}, CI: CIGreen},
		},
	}

	var out strings.Builder
	WriteReport(&out, res)
	report := out.String()

	for _, want := range []string{
		"=== ACTIONABLE (0)",
		"(none)",
		"repos_audited:      1  (active)",
		"archived_skipped:   195",
		"195 archived repos were SKIPPED, not measured",
		"UNKNOWN, not zero",
		"-include-archived",
		"3 open Dependabot PR(s) sit on those repos, mergeable once unarchived.",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q\n---\n%s", want, report)
		}
	}
	if strings.Contains(report, "FROZEN") {
		t.Errorf("skipped archived repos must not produce a FROZEN tier\n---\n%s", report)
	}
}

// An empty tier must read as MEASURED none, never as a section that failed to
// render, and audited archived repos still get the FROZEN tier.
func TestWriteReportWithArchivedIncluded(t *testing.T) {
	res := &Result{
		Owner:           "someone",
		Waivers:         ParseWaivers(""),
		IncludeArchived: true,
		Archived:        1,
		Rows: []Row{
			{Name: "clean", Alerts: AlertState{Count: 0}, CI: CIGreen},
			{Name: "old", Archived: true, Alerts: AlertState{Label: AlertsUnreadable}, CI: CIGreen},
		},
	}

	var out strings.Builder
	WriteReport(&out, res)
	report := out.String()

	for _, want := range []string{
		"=== FROZEN (1 archived)",
		"UNREADABLE on all 1 (403). UNKNOWN, not zero.",
		"repos_audited:      2  (active=1 archived=1)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q\n---\n%s", want, report)
		}
	}
	if strings.Contains(report, "archived_skipped") {
		t.Error("nothing was skipped, so the report must not claim it was")
	}
}

// An account with no archived repos should say nothing about archived repos.
func TestWriteReportOmitsArchivedNoteWhenNoneExist(t *testing.T) {
	res := &Result{Owner: "someone", Waivers: ParseWaivers(""), Rows: []Row{{Name: "only", CI: CIGreen}}}

	var out strings.Builder
	WriteReport(&out, res)

	if report := out.String(); strings.Contains(report, "archived") && !strings.Contains(report, "common on archived repos") {
		t.Errorf("unexpected archived commentary\n---\n%s", report)
	}
}

// An archived repo must never be rendered as having zero advisories: that turns
// unknown into clean, the most damaging error in this domain.
func TestRowTSVRendersLabelsNotZero(t *testing.T) {
	row := Row{Name: "old", Archived: true, Alerts: AlertState{Label: AlertsUnreadable}, CI: CINoCI}
	if want := "old\ttrue\t0\t0\tUNREADABLE\tNO_CI\t"; row.TSV() != want {
		t.Errorf("TSV() = %q, want %q", row.TSV(), want)
	}
}

func TestValidateRejectsForksOnTheBatchedPath(t *testing.T) {
	// The sweep query pins isFork:false, so this combination would report every
	// fork as ERROR rather than auditing it.
	opts := DefaultOptions()
	opts.Owner = "someone"
	opts.IncludeForks = true
	if err := opts.Validate(); err == nil {
		t.Error("expected -include-forks with the graphql source to be rejected")
	}

	opts.CISource = CISourceREST
	if err := opts.Validate(); err != nil {
		t.Errorf("forks via REST should be allowed, got %v", err)
	}
}

func TestDetailMergesAlertAndCIColumns(t *testing.T) {
	row := Row{Alerts: AlertState{Count: 1, Detail: "high:lodash"}, CIDetail: "build=failure"}
	if want := "high:lodash | build=failure"; row.Detail() != want {
		t.Errorf("Detail() = %q, want %q", row.Detail(), want)
	}
	bare := Row{CIDetail: "build=failure"}
	if bare.Detail() != "build=failure" {
		t.Errorf("Detail() = %q, want no leading separator", bare.Detail())
	}
}

func rowNames(rows []Row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}
