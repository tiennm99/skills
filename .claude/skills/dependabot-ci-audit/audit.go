// Package audit produces an account-wide, READ-ONLY picture of Dependabot and
// GitHub Actions CI health.
//
// It reports and diagnoses. It never merges a PR, pushes a commit, changes
// archive state, deletes a workflow run, edits a dependency, or touches a
// repository setting -- and it must stay that way: every caller and every
// downstream report is written on the assumption that running this changes
// nothing.
//
// The correctness rules it exists to enforce are documented at their point of
// use: alerts on an archived repo are UNREADABLE, never zero (alerts.go); the
// updater check-run is not application CI, and only the latest commit is judged
// (ci_classify.go); commit statuses matter as much as check-runs (ci_rest.go and
// queries/ci-sweep.graphql); and "no alerts" differs from "alerts disabled"
// (alerts.go, waivers.go).
package audit

import (
	"context"
	"fmt"

	"dependabot-ci-audit/internal/ghapi"
)

// CI source names. Both paths share one classifier (ci_classify.go), so they can
// only disagree about what they FETCH, never about what a fetch means.
const (
	// CISourceGraphQL is one batched sweep for every repo. The default.
	CISourceGraphQL = "graphql"
	// CISourceREST is 3 REST calls per repo: the independent second opinion, and
	// the only path that can audit forks.
	CISourceREST = "rest"
)

// Options configures a run. The zero value is not usable; see DefaultOptions.
type Options struct {
	Owner string
	// Limit caps how many IN-SCOPE repos are audited, most recently pushed first.
	Limit    int
	CISource string
	// IncludeForks and IncludeArchived widen the scope; both default to false.
	// Archived repos are skipped because nothing on them can be acted on without
	// unarchiving, which this tool does not do.
	IncludeForks    bool
	IncludeArchived bool
	Concurrency     int
	Waivers         Waivers
}

// DefaultOptions returns the settings a plain audit uses.
func DefaultOptions() Options {
	return Options{
		Limit:       1000,
		CISource:    CISourceGraphQL,
		Concurrency: 8,
		Waivers:     ParseWaivers(DefaultWaiverList),
	}
}

// Validate rejects combinations that would silently produce a wrong answer
// rather than an error.
func (o Options) Validate() error {
	if o.Owner == "" {
		return fmt.Errorf("owner is required")
	}
	if o.Limit <= 0 {
		return fmt.Errorf("limit must be positive, got %d", o.Limit)
	}
	if o.Concurrency <= 0 {
		return fmt.Errorf("concurrency must be positive, got %d", o.Concurrency)
	}
	switch o.CISource {
	case CISourceGraphQL:
		// The sweep query pins isFork:false, so it cannot supply CI state for
		// forks. Failing here beats reporting every fork as ERROR.
		if o.IncludeForks {
			return fmt.Errorf("auditing forks requires -ci-source %s: the batched sweep excludes them", CISourceREST)
		}
	case CISourceREST:
	default:
		return fmt.Errorf("ci-source must be %q or %q, got %q", CISourceGraphQL, CISourceREST, o.CISource)
	}
	return nil
}

// Row is one repo's complete audit state.
type Row struct {
	Name          string
	Archived      bool
	DependabotPRs int
	OtherPRs      int
	Alerts        AlertState
	CI            CIState
	CIDetail      string
}

// Detail merges the alert and CI details into the report's last column.
func (r Row) Detail() string {
	if r.Alerts.Detail == "" {
		return r.CIDetail
	}
	return r.Alerts.Detail + " | " + r.CIDetail
}

// TSV renders the row in the report's column order:
// repo, archived, dependabot_prs, other_prs, alerts, ci_state, detail.
func (r Row) TSV() string {
	return fmt.Sprintf("%s\t%t\t%d\t%d\t%s\t%s\t%s",
		r.Name, r.Archived, r.DependabotPRs, r.OtherPRs, r.Alerts, r.CI, r.Detail())
}

// IsFinding reports whether an ACTIVE repo needs action. Archived repos are
// never findings -- see Result.Tiers.
//
// waived suppresses Dependabot-state findings only. What survives a waiver:
//   - open Dependabot PRs, which are directly mergeable, and whose existence
//     contradicts the premise that Dependabot is off
//   - BUILD_FAILED and STUCK, which are the project's own CI and third-party
//     statuses, nothing to do with Dependabot
func (r Row) IsFinding(waived bool) bool {
	if r.DependabotPRs > 0 {
		return true
	}
	switch {
	// Alerts switched off on a live repo is real, unmeasured exposure. ERROR is
	// equally unknown. DISABLED_OK is the one label the operator has accepted.
	case r.Alerts.Label == AlertsDisabled, r.Alerts.Label == AlertsError:
		return true
	case r.Alerts.Measured() && r.Alerts.Count > 0:
		return true
	}
	switch r.CI {
	case CIGreen, CINoCI, CINoCommits:
		return false
	case CIDependabotJob:
		// With the updater off by intent its leftover check-runs cannot re-run,
		// so they are declared noise rather than a finding.
		return !waived
	default:
		return true
	}
}

// Result is everything one run measured. Rows are in inventory order (most
// recently pushed first) and cover every repo in scope, not just findings.
type Result struct {
	Owner           string
	CISource        string
	IncludeForks    bool
	IncludeArchived bool
	Rows            []Row
	// Forks and Archived count what the account HAS, whether or not it was
	// audited, so the summary can state what went unmeasured.
	Forks    int
	Archived int
	// SkippedDependabotPRs counts open Dependabot PRs on archived repos that were
	// skipped. Reported anyway: unarchiving makes them mergeable, so they must not
	// disappear along with the repo.
	SkippedDependabotPRs int
	Waivers              Waivers

	TotalDependabotPRs int
	TotalOtherPRs      int
}

// ArchivedAudited counts archived repos that are actually in the rows, which is
// zero unless -include-archived was passed.
func (res *Result) ArchivedAudited() int {
	n := 0
	for _, r := range res.Rows {
		if r.Archived {
			n++
		}
	}
	return n
}

// Active counts the repos the ACTIONABLE tier is drawn from.
func (res *Result) Active() int { return len(res.Rows) - res.ArchivedAudited() }

// Run performs the audit. It makes no writes of any kind.
func Run(ctx context.Context, client *ghapi.Client, opts Options) (*Result, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	inventory, err := FetchInventory(ctx, client, opts.Owner)
	if err != nil {
		return nil, err
	}
	scope := inventory.InScope(opts.IncludeForks, opts.IncludeArchived, opts.Limit)

	prs, err := FetchOpenPullRequests(ctx, client, opts.Owner)
	if err != nil {
		return nil, err
	}

	alerts := FetchAlerts(ctx, client, opts.Owner, scope.Repos, opts.Waivers, opts.Concurrency)

	var ci map[string]CIResult
	if opts.CISource == CISourceGraphQL {
		if ci, err = SweepCI(ctx, client, opts.Owner); err != nil {
			return nil, err
		}
	} else {
		ci = FetchCIViaREST(ctx, client, opts.Owner, scope.Repos, opts.Concurrency)
	}

	res := &Result{
		Owner:              opts.Owner,
		CISource:           opts.CISource,
		IncludeForks:       opts.IncludeForks,
		IncludeArchived:    opts.IncludeArchived,
		Forks:              scope.Forks,
		Archived:           scope.Archived,
		Waivers:            opts.Waivers,
		TotalDependabotPRs: prs.TotalDependabot,
		TotalOtherPRs:      prs.TotalOther,
		Rows:               make([]Row, 0, len(scope.Repos)),
	}
	for _, name := range scope.ArchivedSkipped {
		res.SkippedDependabotPRs += prs.Dependabot[name]
	}
	for _, repo := range scope.Repos {
		state, ok := ci[repo.Name]
		if !ok {
			// A repo absent from the sweep is unknown, never a silent GREEN.
			state = CIResult{State: CIError, Detail: "no CI data returned for this repo"}
		}
		alert, ok := alerts[repo.Name]
		if !ok {
			alert = AlertState{Label: AlertsError, Detail: "no alert data returned for this repo"}
		}
		res.Rows = append(res.Rows, Row{
			Name:          repo.Name,
			Archived:      repo.IsArchived,
			DependabotPRs: prs.Dependabot[repo.Name],
			OtherPRs:      prs.Other[repo.Name],
			Alerts:        alert,
			CI:            state.State,
			CIDetail:      state.Detail,
		})
	}
	return res, nil
}
