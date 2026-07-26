package audit

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dependabot-ci-audit/internal/ghapi"
	"github.com/google/go-github/v89/github"
)

// dependabotAuthor matches the bot's login AFTER normalization.
//
// The login differs per API: search reports "dependabot[bot]", GraphQL-backed
// calls report "app/dependabot". Matching only one form counts every Dependabot
// PR as a human PR -- and a repo whose sole finding is Dependabot PRs then
// emits no row at all, i.e. reads as clean.
//
// Do NOT switch to a bot-type field instead: gh 2.92.0 reports is_bot=false for
// authors it simultaneously types as "Bot", and the same underlying data feeds
// this API.
var dependabotAuthor = regexp.MustCompile(`^dependabot(-preview)?$`)

// PullRequests counts open PRs per repo, split by author.
type PullRequests struct {
	Dependabot      map[string]int
	Other           map[string]int
	TotalDependabot int
	TotalOther      int
}

// FetchOpenPullRequests gets every open PR the owner has in ONE search, rather
// than one list call per repo. On hundreds of repos that is the difference
// between 1 call and N.
//
// A search failure is FATAL. Tolerating it would report zero Dependabot PRs for
// the whole account, and "no open Dependabot PRs" is the most reassuring
// sentence this audit can print -- it must never be the consequence of a failed
// call.
func FetchOpenPullRequests(ctx context.Context, client *ghapi.Client, owner string) (*PullRequests, error) {
	prs := &PullRequests{Dependabot: map[string]int{}, Other: map[string]int{}}
	query := fmt.Sprintf("is:pr is:open user:%s", owner)
	opts := &github.SearchOptions{ListOptions: github.ListOptions{PerPage: 100}}

	for {
		var result *github.IssuesSearchResult
		var resp *github.Response
		err := ghapi.Retry(ctx, 4, func() error {
			var err error
			result, resp, err = client.Search.Issues(ctx, query, opts)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("searching open PRs for %s: %w", owner, err)
		}

		for _, issue := range result.Issues {
			repo := repoNameFromURL(issue.GetRepositoryURL())
			if repo == "" {
				return nil, fmt.Errorf("cannot tell which repo PR #%d belongs to", issue.GetNumber())
			}
			if dependabotAuthor.MatchString(normalizeLogin(issue.GetUser().GetLogin())) {
				prs.Dependabot[repo]++
				prs.TotalDependabot++
				continue
			}
			prs.Other[repo]++
			prs.TotalOther++
		}

		// The search API caps out at 1000 results and stops advertising a next
		// page there, so exhausting the pages is not proof of completeness. Both
		// ceilings are reported rather than passed off as a total.
		if resp.NextPage == 0 {
			if result.GetIncompleteResults() {
				return nil, fmt.Errorf(
					"PR search timed out server-side and returned partial results; re-run before trusting PR counts")
			}
			if seen := prs.TotalDependabot + prs.TotalOther; result.GetTotal() > seen {
				return nil, fmt.Errorf(
					"PR search matched %d open PRs but only %d were returned (1000-result API ceiling); counts would be understated",
					result.GetTotal(), seen)
			}
			return prs, nil
		}
		opts.Page = resp.NextPage
	}
}

// normalizeLogin strips the two decorations GitHub applies to app identities so
// "app/dependabot" and "dependabot[bot]" compare equal.
func normalizeLogin(login string) string {
	return strings.TrimSuffix(strings.TrimPrefix(login, "app/"), "[bot]")
}

// repoNameFromURL pulls the repo name out of an API repository_url, which is
// the only repo identity a search result carries.
func repoNameFromURL(url string) string {
	if i := strings.LastIndexByte(url, '/'); i >= 0 {
		return url[i+1:]
	}
	return ""
}
