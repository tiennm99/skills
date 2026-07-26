// Command audit-repos runs an account-wide, READ-ONLY Dependabot and GitHub
// Actions CI audit and prints a tiered report.
//
// It never merges, commits, changes archive state, deletes runs, or edits
// settings. Exit status is 0 for any completed audit, including one that found
// something: a non-zero exit is reserved for a FAILED audit, so "it exited
// non-zero" always means the numbers cannot be trusted. Findings live in the
// ACTIONABLE tier of the report.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	audit "dependabot-ci-audit"
	"dependabot-ci-audit/internal/ghapi"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	defaults := audit.DefaultOptions()

	var (
		owner        = flag.String("owner", "", "repository owner; defaults to the authenticated user (also accepted as a positional argument)")
		limit        = flag.Int("limit", defaults.Limit, "audit at most N repos, most recently pushed first; applied before forks are filtered out")
		ciSource     = flag.String("ci-source", defaults.CISource, "where CI state comes from: graphql (one batched sweep) or rest (3 calls per repo, independent second opinion)")
		includeForks = flag.Bool("include-forks", false, "audit forks too; requires -ci-source rest")
		verifyRepo   = flag.String("verify-repo", "", "audit ONE repo through the REST path and print its classification")
		emitAll      = flag.Bool("emit-all", false, "print every audited repo as a flat row instead of tiers, for diffing runs")
		concurrency  = flag.Int("concurrency", defaults.Concurrency, "per-repo calls in flight; higher is faster until GitHub's secondary rate limit pushes back")
		waiverFile   = flag.String("waiver-file", "", "read waived repos from this file instead of the compiled-in waivers.txt")
		noWaivers    = flag.Bool("no-waivers", false, "waive nothing, so every repo's Dependabot state is measured")
	)
	flag.Usage = usage
	flag.Parse()

	// Whether -ci-source was typed matters: -verify-repo IS the REST path, so
	// asking for both it and graphql is a contradiction, while leaving the
	// default in place is not.
	ciSourceSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "ci-source" {
			ciSourceSet = true
		}
	})

	opts := defaults
	opts.Owner = *owner
	if opts.Owner == "" {
		opts.Owner = flag.Arg(0)
	}
	opts.Limit = *limit
	opts.CISource = *ciSource
	opts.IncludeForks = *includeForks
	opts.Concurrency = *concurrency

	waivers, err := loadWaivers(*waiverFile, *noWaivers)
	if err != nil {
		return err
	}
	opts.Waivers = waivers

	// Ctrl-C cancels in-flight calls rather than leaving the process to finish a
	// sweep nobody is waiting for.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := ghapi.New(ctx)
	if err != nil {
		return err
	}
	if opts.Owner == "" {
		if opts.Owner, err = client.Login(ctx); err != nil {
			return err
		}
	}

	if *verifyRepo != "" {
		if ciSourceSet && opts.CISource == audit.CISourceGraphQL {
			return fmt.Errorf("-verify-repo is the REST spot-check; it cannot be combined with -ci-source %s", audit.CISourceGraphQL)
		}
		return spotCheck(ctx, client, opts, *verifyRepo)
	}

	result, err := audit.Run(ctx, client, opts)
	if err != nil {
		return err
	}
	if *emitAll {
		audit.WriteFlat(os.Stdout, result)
		return nil
	}
	audit.WriteReport(os.Stdout, result)
	return nil
}

// loadWaivers resolves which repos have their Dependabot findings waived.
func loadWaivers(path string, none bool) (audit.Waivers, error) {
	switch {
	case none && path != "":
		return audit.Waivers{}, fmt.Errorf("-no-waivers and -waiver-file contradict each other")
	case none:
		return audit.ParseWaivers(""), nil
	case path != "":
		text, err := os.ReadFile(path)
		if err != nil {
			// Falling back to the compiled-in list here would silently change
			// which repos are measured, so an unreadable file is fatal.
			return audit.Waivers{}, fmt.Errorf("reading the waiver file: %w", err)
		}
		return audit.ParseWaivers(string(text)), nil
	default:
		return audit.ParseWaivers(audit.DefaultWaiverList), nil
	}
}

// spotCheck audits a single repo through the REST path.
//
// This is the independent second opinion for one repo, and the reason it exists
// is that a REST/GraphQL disagreement on the same commit is what exposed the
// statusCheckRollup blind spot.
func spotCheck(ctx context.Context, client *ghapi.Client, opts audit.Options, name string) error {
	repo, _, err := client.Repositories.Get(ctx, opts.Owner, name)
	if err != nil {
		return fmt.Errorf("cannot read %s/%s: %w", opts.Owner, name, err)
	}

	target := []audit.Repo{{
		Name:          repo.GetName(),
		DefaultBranch: repo.GetDefaultBranch(),
		IsArchived:    repo.GetArchived(),
		IsFork:        repo.GetFork(),
	}}

	alerts := audit.FetchAlerts(ctx, client, opts.Owner, target, opts.Waivers, 1)
	ci := audit.FetchCIViaREST(ctx, client, opts.Owner, target, 1)

	audit.WriteSpotCheck(os.Stdout, audit.Row{
		Name:     target[0].Name,
		Archived: target[0].IsArchived,
		Alerts:   alerts[target[0].Name],
		CI:       ci[target[0].Name].State,
		CIDetail: ci[target[0].Name].Detail,
	})
	return nil
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprint(out, strings.TrimLeft(`
audit-repos - READ-ONLY Dependabot and GitHub Actions CI audit across every repo
an owner has. Reports and diagnoses; never merges, commits, or changes settings.

Usage:
  audit-repos [flags] [owner]

Examples:
  audit-repos                              audit the authenticated user
  audit-repos some-org                     audit an organization
  audit-repos -limit 50 some-org           quick sample of the 50 newest-pushed
  audit-repos -ci-source rest some-org     whole audit via the per-repo path
  audit-repos -verify-repo name some-org   REST spot-check of one repo
  audit-repos -include-forks -ci-source rest some-org

Authentication comes from GH_TOKEN, GITHUB_TOKEN, or `+"`gh auth token`"+`, in that
order. Nothing else is read from the environment.

Flags:
`, "\n"))
	flag.PrintDefaults()
}
