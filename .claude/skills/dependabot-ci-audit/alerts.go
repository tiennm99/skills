package audit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"dependabot-ci-audit/internal/ghapi"
	"github.com/google/go-github/v89/github"
)

// AlertLabel is a non-numeric alert outcome. Every one of them means UNMEASURED.
//
// Only a number is a measurement. Collapsing any of these into "0" turns
// unknown into clean, the most damaging error in this domain, so they are kept
// distinct all the way to the report.
type AlertLabel string

const (
	// AlertsUnreadable is an archived repo: the endpoint always 403s.
	AlertsUnreadable AlertLabel = "UNREADABLE"
	// AlertsDisabled means Dependabot alerts are switched off, so nothing is
	// being detected. On an active repo that is itself a finding.
	AlertsDisabled AlertLabel = "DISABLED"
	// AlertsDisabledOK is AlertsDisabled that the operator declared intentional.
	AlertsDisabledOK AlertLabel = "DISABLED_OK"
	// AlertsError is any other failure. Unknown, never clean.
	AlertsError AlertLabel = "ERROR"
)

// AlertState is either a count or a label, never both.
type AlertState struct {
	Count  int
	Label  AlertLabel
	Detail string
}

// Measured reports whether Count means anything.
func (a AlertState) Measured() bool { return a.Label == "" }

func (a AlertState) String() string {
	if a.Measured() {
		return fmt.Sprint(a.Count)
	}
	return string(a.Label)
}

// FetchAlerts reads open Dependabot alerts for every repo in scope.
//
// This is the one genuinely per-repo call in the audit; it runs concurrently
// because it is otherwise the whole runtime. Failures are recorded as states
// rather than returned, so one unreadable repo never aborts the sweep -- but
// they are recorded as ERROR, never as zero.
func FetchAlerts(ctx context.Context, client *ghapi.Client, owner string, repos []Repo, waivers Waivers, concurrency int) map[string]AlertState {
	return mapRepos(ctx, repos, concurrency, func(r Repo) AlertState {
		// Archived is checked FIRST so a waived repo that is also archived still
		// counts toward the archived blind spot, rather than looking like a
		// state someone deliberately accepted.
		if r.IsArchived {
			return AlertState{Label: AlertsUnreadable}
		}
		if waivers.Has(r.Name) {
			return AlertState{Label: AlertsDisabledOK, Detail: "not checked: alerts disabled by intent"}
		}
		return fetchRepoAlerts(ctx, client, owner, r.Name)
	})
}

func fetchRepoAlerts(ctx context.Context, client *ghapi.Client, owner, repo string) AlertState {
	// Paginated on purpose: a repo can exceed one 100-item page, and a silent
	// truncation would understate exposure.
	//
	// ListOptions is named explicitly because ListAlertsOptions embeds both it
	// and ListCursorOptions, which makes a bare .PerPage ambiguous.
	opts := &github.ListAlertsOptions{State: github.Ptr("open")}
	opts.ListOptions.PerPage = 100

	var all []*github.DependabotAlert
	for {
		var page []*github.DependabotAlert
		var resp *github.Response
		err := ghapi.Retry(ctx, 4, func() error {
			var err error
			page, resp, err = client.Dependabot.ListRepoAlerts(ctx, owner, repo, opts)
			return err
		})
		if err != nil {
			return classifyAlertsError(err)
		}
		all = append(all, page...)
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return AlertState{Count: len(all), Detail: advisorySummary(all)}
}

// classifyAlertsError separates the two 403s that mean something specific from
// everything else.
//
// GitHub answers this endpoint with 403 both for an archived repo and for one
// with alerts switched off, and the distinction changes the verdict: one is a
// blind spot that needs unarchiving, the other is live exposure nobody is
// watching. go-github surfaces the message, so this reads a typed field instead
// of grepping a response body.
func classifyAlertsError(err error) AlertState {
	var apiErr *github.ErrorResponse
	if errors.As(err, &apiErr) {
		message := strings.ToLower(apiErr.Message)
		switch {
		case strings.Contains(message, "archived"):
			return AlertState{Label: AlertsUnreadable}
		case strings.Contains(message, "disabled"):
			return AlertState{Label: AlertsDisabled}
		}
	}
	return AlertState{Label: AlertsError, Detail: firstLine(err.Error())}
}

// advisorySummary lists each distinct severity:package once, sorted, so the
// detail column is stable between runs and between repos.
func advisorySummary(alerts []*github.DependabotAlert) string {
	pairs := make([]string, 0, len(alerts))
	for _, a := range alerts {
		severity := a.GetSecurityAdvisory().GetSeverity()
		if severity == "" {
			severity = "unknown"
		}
		pairs = append(pairs, severity+":"+a.GetDependency().GetPackage().GetName())
	}
	sort.Strings(pairs)
	return strings.Join(slices.Compact(pairs), ", ")
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
