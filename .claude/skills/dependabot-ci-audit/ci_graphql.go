package audit

import (
	"context"
	"fmt"
	"strings"

	"dependabot-ci-audit/internal/ghapi"
)

// CIResult is one repo's CI verdict plus the checks behind it.
type CIResult struct {
	State  CIState
	Detail string
}

type sweepPage struct {
	RepositoryOwner *struct {
		Repositories struct {
			TotalCount int `json:"totalCount"`
			PageInfo   struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []sweepRepo `json:"nodes"`
		} `json:"repositories"`
	} `json:"repositoryOwner"`
}

type sweepRepo struct {
	Name             string `json:"name"`
	NameWithOwner    string `json:"nameWithOwner"`
	IsArchived       bool   `json:"isArchived"`
	DefaultBranchRef *struct {
		Name   string `json:"name"`
		Target *struct {
			OID    string `json:"oid"`
			Status *struct {
				Contexts []struct {
					Context string `json:"context"`
					State   string `json:"state"`
				} `json:"contexts"`
			} `json:"status"`
			CheckSuites struct {
				TotalCount int `json:"totalCount"`
				Nodes      []struct {
					CheckRuns struct {
						TotalCount int `json:"totalCount"`
						Nodes      []struct {
							Name       string `json:"name"`
							Conclusion string `json:"conclusion"`
							Status     string `json:"status"`
						} `json:"nodes"`
					} `json:"checkRuns"`
				} `json:"nodes"`
			} `json:"checkSuites"`
		} `json:"target"`
	} `json:"defaultBranchRef"`
}

// SweepCI gets CI state for every non-fork repo in one paginated query, instead
// of 3 REST calls per repo.
//
// Every validation below is FATAL on purpose. A sweep that returns nothing, or
// half of the repos, would classify the rest as NO_CI -- rendering "unknown" as
// "all green". Partial results are refused rather than reported.
func SweepCI(ctx context.Context, client *ghapi.Client, owner string) (map[string]CIResult, error) {
	results := map[string]CIResult{}
	var truncated []string
	expected := -1
	prefix := owner + "/"
	cursor := ""

	for {
		vars := map[string]any{"owner": owner}
		if cursor != "" {
			vars["endCursor"] = cursor
		}

		var page sweepPage
		err := ghapi.Retry(ctx, 4, func() error {
			return client.GraphQL(ctx, ciSweepQuery, vars, &page)
		})
		if err != nil {
			return nil, fmt.Errorf("CI sweep failed, so no results are reported: %w", err)
		}
		if page.RepositoryOwner == nil {
			return nil, fmt.Errorf("CI sweep found no such user or organization: %s", owner)
		}

		repos := page.RepositoryOwner.Repositories
		if expected < 0 {
			expected = repos.TotalCount
		}
		for _, node := range repos.Nodes {
			// Repos owned by another account mean the affiliation pins were lost.
			if !strings.HasPrefix(node.NameWithOwner, prefix) {
				return nil, fmt.Errorf(
					"CI sweep returned %s, which %s does not own (affiliation pins lost)",
					node.NameWithOwner, owner)
			}
			state, detail, short := classifySweepRepo(node)
			if short {
				truncated = append(truncated, node.Name)
			}
			results[node.Name] = CIResult{State: state, Detail: detail}
		}

		if !repos.PageInfo.HasNextPage {
			break
		}
		cursor = repos.PageInfo.EndCursor
	}

	if expected <= 0 {
		return nil, fmt.Errorf("CI sweep returned no repositories for %s", owner)
	}
	if len(results) != expected {
		return nil, fmt.Errorf(
			"CI sweep incomplete: totalCount=%d but %d unique repos returned", expected, len(results))
	}
	// A short page is an unknown, not a measurement.
	if len(truncated) > 0 {
		return nil, fmt.Errorf(
			"check data truncated for %s; raise the page size in queries/ci-sweep.graphql before trusting results",
			strings.Join(truncated, ", "))
	}
	return results, nil
}

// classifySweepRepo flattens one sweep node into checks and classifies it. The
// third return reports whether a page came back short, which the caller treats
// as fatal.
//
// Commit statuses come first so the detail column orders identically to the REST
// path. Both APIs are required: `status` carries third-party statuses (Vercel,
// Cloudflare) that never appear as check-runs, `checkSuites` carries Actions.
func classifySweepRepo(node sweepRepo) (CIState, string, bool) {
	if node.DefaultBranchRef == nil {
		return CINoCommits, "", false
	}
	target := node.DefaultBranchRef.Target
	// A default branch pointing at a tag or tree rather than a commit: there is
	// no check state to read, which is not the same as an empty repo.
	if target == nil {
		return CINoCI, "", false
	}

	var checks []Check
	if target.Status != nil {
		for _, c := range target.Status.Contexts {
			checks = append(checks, NormalizeCheck(c.Context, c.State, ""))
		}
	}
	short := target.CheckSuites.TotalCount > len(target.CheckSuites.Nodes)
	for _, suite := range target.CheckSuites.Nodes {
		if suite.CheckRuns.TotalCount > len(suite.CheckRuns.Nodes) {
			short = true
		}
		for _, run := range suite.CheckRuns.Nodes {
			checks = append(checks, NormalizeCheck(run.Name, run.Conclusion, run.Status))
		}
	}

	state, detail := Classify(true, checks)
	return state, detail, short
}
