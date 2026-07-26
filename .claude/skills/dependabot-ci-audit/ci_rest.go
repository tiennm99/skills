package audit

import (
	"context"
	"fmt"

	"dependabot-ci-audit/internal/ghapi"
	"github.com/google/go-github/v89/github"
)

// FetchCIViaREST is the independent second opinion on CI state: 3 calls per
// repo instead of one batched sweep.
//
// Slow by construction, and kept anyway. A REST/GraphQL disagreement on the same
// commit is what exposed the statusCheckRollup blind spot, so this path is the
// evidence that the fast path is still correct. It is also the only way to audit
// forks, which the sweep query excludes.
func FetchCIViaREST(ctx context.Context, client *ghapi.Client, owner string, repos []Repo, concurrency int) map[string]CIResult {
	return mapRepos(ctx, repos, concurrency, func(r Repo) CIResult {
		return fetchRepoCI(ctx, client, owner, r)
	})
}

func fetchRepoCI(ctx context.Context, client *ghapi.Client, owner string, repo Repo) CIResult {
	// Empty repo: the inventory already told us there is no default branch, so
	// there is nothing to build and nothing to judge.
	if repo.DefaultBranch == "" {
		return CIResult{State: CINoCommits}
	}

	sha, err := headSHA(ctx, client, owner, repo)
	if err != nil {
		return CIResult{State: CIError, Detail: firstLine(err.Error())}
	}

	// Statuses first, matching the sweep's detail ordering.
	checks, err := commitStatuses(ctx, client, owner, repo.Name, sha)
	if err != nil {
		return CIResult{State: CIError, Detail: firstLine(err.Error())}
	}
	runs, err := checkRuns(ctx, client, owner, repo.Name, sha)
	if err != nil {
		return CIResult{State: CIError, Detail: firstLine(err.Error())}
	}
	checks = append(checks, runs...)

	state, detail := Classify(true, checks)
	return CIResult{State: state, Detail: detail}
}

func headSHA(ctx context.Context, client *ghapi.Client, owner string, repo Repo) (string, error) {
	var commit *github.RepositoryCommit
	err := ghapi.Retry(ctx, 4, func() error {
		var err error
		commit, _, err = client.Repositories.GetCommit(ctx, owner, repo.Name, repo.DefaultBranch,
			&github.ListOptions{PerPage: 1})
		return err
	})
	if err != nil {
		return "", fmt.Errorf("reading %s@%s: %w", repo.Name, repo.DefaultBranch, err)
	}
	if commit.GetSHA() == "" {
		return "", fmt.Errorf("%s@%s has no head sha", repo.Name, repo.DefaultBranch)
	}
	return commit.GetSHA(), nil
}

// commitStatuses reads the combined status: Vercel, Cloudflare and similar
// integrations report here and never as check-runs, so a repo can look NO_CI
// while carrying a pending status that will never resolve.
func commitStatuses(ctx context.Context, client *ghapi.Client, owner, repo, sha string) ([]Check, error) {
	opts := &github.ListOptions{PerPage: 100}
	var checks []Check
	for {
		var combined *github.CombinedStatus
		var resp *github.Response
		err := ghapi.Retry(ctx, 4, func() error {
			var err error
			combined, resp, err = client.Repositories.GetCombinedStatus(ctx, owner, repo, sha, opts)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("reading commit statuses for %s: %w", repo, err)
		}
		for _, s := range combined.Statuses {
			checks = append(checks, NormalizeCheck(s.GetContext(), s.GetState(), ""))
		}
		if resp.NextPage == 0 {
			return checks, nil
		}
		opts.Page = resp.NextPage
	}
}

// checkRuns reads Actions results. Paginated: the default page is 30, and a repo
// with more check-runs than that would otherwise be classified on a partial view
// -- which can hide the very failure being looked for.
func checkRuns(ctx context.Context, client *ghapi.Client, owner, repo, sha string) ([]Check, error) {
	opts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	var checks []Check
	for {
		var result *github.ListCheckRunsResults
		var resp *github.Response
		err := ghapi.Retry(ctx, 4, func() error {
			var err error
			result, resp, err = client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("reading check-runs for %s: %w", repo, err)
		}
		for _, run := range result.CheckRuns {
			checks = append(checks, NormalizeCheck(run.GetName(), run.GetConclusion(), run.GetStatus()))
		}
		if resp.NextPage == 0 {
			return checks, nil
		}
		opts.Page = resp.NextPage
	}
}
