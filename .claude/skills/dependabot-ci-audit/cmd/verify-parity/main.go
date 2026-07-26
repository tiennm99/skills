// Command verify-parity proves the batched GraphQL CI sweep classifies
// identically to the per-repo REST path.
//
// This is not ceremony. The only reason the statusCheckRollup blind spot was
// ever found is that these two paths disagreed on the same commit. Run it after
// ANY edit to queries/ci-sweep.graphql, to the sweep decoder, or to the shared
// classifier.
//
// Exit 0 only when every repo in the slice classifies the same on both paths.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"

	audit "dependabot-ci-audit"
	"dependabot-ci-audit/internal/ghapi"
)

// interestingDefault names repos that exercise the cases most likely to
// diverge: waived repos, ones carrying updater check-runs, and ones whose CI
// reports through commit statuses rather than check-runs.
//
// Their presence in the slice is ASSERTED, not assumed. The slice is ordered by
// push date, which reorders as repos are pushed to -- these moved 46/47 -> 51/52
// within a single session, so a fixed limit silently stops covering them.
const interestingDefault = "claudekit-engineer,claudekit-marketing,chambai,exchange-rate-export"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	defaults := audit.DefaultOptions()
	var (
		owner       = flag.String("owner", "", "repository owner; defaults to the authenticated user (also accepted as a positional argument)")
		limit       = flag.Int("limit", 50, "compare the N most recently pushed repos")
		concurrency = flag.Int("concurrency", defaults.Concurrency, "per-repo calls in flight")
		interesting = flag.String("interesting", interestingDefault, "comma-separated repos the slice must cover; a clean diff over the wrong repos proves nothing")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := ghapi.New(ctx)
	if err != nil {
		return err
	}

	opts := defaults
	opts.Owner = *owner
	if opts.Owner == "" {
		opts.Owner = flag.Arg(0)
	}
	if opts.Owner == "" {
		if opts.Owner, err = client.Login(ctx); err != nil {
			return err
		}
	}
	opts.Limit = *limit
	opts.Concurrency = *concurrency

	fmt.Printf("== gate: %s, slice=%d ==\n", opts.Owner, opts.Limit)

	fmt.Println("-- REST path --")
	restOpts := opts
	restOpts.CISource = audit.CISourceREST
	restResult, err := audit.Run(ctx, client, restOpts)
	if err != nil {
		return fmt.Errorf("REST path: %w", err)
	}

	fmt.Println("-- GraphQL path --")
	gqlOpts := opts
	gqlOpts.CISource = audit.CISourceGraphQL
	gqlResult, err := audit.Run(ctx, client, gqlOpts)
	if err != nil {
		return fmt.Errorf("GraphQL path: %w", err)
	}

	// Both runs must have audited the same repos, or the diff below compares
	// different populations. Push dates can change between the two runs, so this
	// is checked rather than assumed.
	restNames := names(restResult)
	gqlNames := names(gqlResult)
	if !slices.Equal(restNames, gqlNames) {
		return fmt.Errorf("the two runs audited different repos (%d vs %d); repos were pushed to mid-gate, so re-run",
			len(restNames), len(gqlNames))
	}
	// A diff of two empty sets "passes" while proving nothing. Refuse it.
	if len(restNames) == 0 {
		return fmt.Errorf("no repos were audited, so a clean diff would be vacuous")
	}

	if missing := missingFrom(restNames, *interesting); len(missing) > 0 {
		return fmt.Errorf("slice of %d does not cover: %s\n"+
			"      raise -limit until it does -- a clean diff over the wrong repos proves nothing",
			opts.Limit, strings.Join(missing, " "))
	}
	fmt.Println("-- slice covers all interesting repos --")

	// Classification must be exercised, not merely uniform: an all-NO_CI slice
	// would agree trivially on both paths.
	fmt.Printf("-- ci_states exercised: %s\n", strings.Join(statesExercised(restResult), " "))

	fmt.Printf("-- diff (repo / alerts / ci_state), %d repos --\n", len(restNames))
	diffs := compare(restResult, gqlResult)
	if len(diffs) == 0 {
		fmt.Printf("\nPASS: identical classification on both paths across %d repos.\n", len(restNames))
		return nil
	}
	for _, line := range diffs {
		fmt.Println(line)
	}
	return fmt.Errorf("paths disagree on %d repo(s). Do NOT trust the GraphQL default until this is empty", len(diffs))
}

// compare diffs the two runs on repo, alerts and ci_state.
//
// The detail column is deliberately excluded: it lists the same checks either
// way, but only the classification changes a verdict, and diffing free text
// would bury a real disagreement in noise.
func compare(rest, gql *audit.Result) []string {
	gqlRows := map[string]audit.Row{}
	for _, row := range gql.Rows {
		gqlRows[row.Name] = row
	}

	var diffs []string
	for _, r := range rest.Rows {
		g := gqlRows[r.Name]
		if r.Alerts.String() == g.Alerts.String() && r.CI == g.CI {
			continue
		}
		diffs = append(diffs,
			fmt.Sprintf("  %s\n    rest:    alerts=%s ci=%s\n    graphql: alerts=%s ci=%s",
				r.Name, r.Alerts, r.CI, g.Alerts, g.CI))
	}
	return diffs
}

func names(res *audit.Result) []string {
	out := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		out = append(out, row.Name)
	}
	sort.Strings(out)
	return out
}

func statesExercised(res *audit.Result) []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range res.Rows {
		if state := string(row.CI); !seen[state] {
			seen[state] = true
			out = append(out, state)
		}
	}
	sort.Strings(out)
	return out
}

func missingFrom(audited []string, interesting string) []string {
	var missing []string
	for _, want := range strings.Split(interesting, ",") {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if _, found := slices.BinarySearch(audited, want); !found {
			missing = append(missing, want)
		}
	}
	return missing
}
