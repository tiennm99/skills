package audit

import (
	"context"
	"fmt"
	"strings"

	"dependabot-ci-audit/internal/ghapi"
)

// Repo is one repository in the owner's inventory.
type Repo struct {
	Name string
	// DefaultBranch is empty on a repo with no commits.
	DefaultBranch string
	IsFork        bool
	IsArchived    bool
}

// Inventory is every repo the owner owns, most recently pushed first. Forks and
// archived repos are included so the caller can count them before dropping them.
type Inventory struct {
	Repos []Repo
}

// Scope is the set of repos to audit plus an exact account of what was left out.
//
// The counts are the point. Dropping repos silently would let the report read as
// clean while whole populations went unmeasured, so every exclusion is carried
// through to the summary.
type Scope struct {
	Repos    []Repo
	Forks    int
	Archived int
	// ArchivedSkipped names the archived repos dropped from Repos. Their open
	// Dependabot PRs are still disclosed: an archived repo's PR becomes mergeable
	// the moment someone unarchives, so it must not vanish with the repo.
	ArchivedSkipped []string
}

type inventoryPage struct {
	RepositoryOwner *struct {
		Repositories struct {
			TotalCount int `json:"totalCount"`
			PageInfo   struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []struct {
				Name             string `json:"name"`
				NameWithOwner    string `json:"nameWithOwner"`
				IsFork           bool   `json:"isFork"`
				IsArchived       bool   `json:"isArchived"`
				DefaultBranchRef *struct {
					Name string `json:"name"`
				} `json:"defaultBranchRef"`
			} `json:"nodes"`
		} `json:"repositories"`
	} `json:"repositoryOwner"`
}

// FetchInventory pages through every repo the owner has.
//
// All pages are fetched even when the caller wants a small slice, because the
// summary has to state exactly how many forks and archived repos were excluded,
// and a truncated inventory could only guess. Three pages of 100 cost about a
// second against a sweep that pages nine times regardless.
func FetchInventory(ctx context.Context, client *ghapi.Client, owner string) (*Inventory, error) {
	inv := &Inventory{}
	cursor := ""
	prefix := owner + "/"
	for {
		vars := map[string]any{"owner": owner}
		if cursor != "" {
			vars["endCursor"] = cursor
		}

		var page inventoryPage
		err := ghapi.Retry(ctx, 4, func() error {
			return client.GraphQL(ctx, inventoryQuery, vars, &page)
		})
		if err != nil {
			return nil, fmt.Errorf("listing repos for %s: %w", owner, err)
		}
		if page.RepositoryOwner == nil {
			return nil, fmt.Errorf("no such user or organization: %s", owner)
		}

		repos := page.RepositoryOwner.Repositories
		for _, node := range repos.Nodes {
			// A repo owned by someone else means the affiliation pins stopped
			// working and the scope has silently widened past this owner.
			if !strings.HasPrefix(node.NameWithOwner, prefix) {
				return nil, fmt.Errorf(
					"inventory returned %s, which %s does not own (affiliation pins lost)",
					node.NameWithOwner, owner)
			}
			r := Repo{Name: node.Name, IsFork: node.IsFork, IsArchived: node.IsArchived}
			if node.DefaultBranchRef != nil {
				r.DefaultBranch = node.DefaultBranchRef.Name
			}
			inv.Repos = append(inv.Repos, r)
		}

		if !repos.PageInfo.HasNextPage {
			break
		}
		cursor = repos.PageInfo.EndCursor
	}

	if len(inv.Repos) == 0 {
		return nil, fmt.Errorf("%s has no repositories visible to this token", owner)
	}
	return inv, nil
}

// InScope selects what to audit and counts what it drops.
//
// Forks are excluded because their alerts and Dependabot PRs belong to the
// upstream project, not to this owner.
//
// Archived repos are excluded because nothing on them is actionable without
// unarchiving: alerts 403, PRs are unmergeable on a read-only repo, and Actions
// are frozen. They are SKIPPED, not judged -- the summary reports the count and
// refuses to describe them as clean, since their advisory state is unreadable
// rather than zero.
//
// limit is applied LAST, so it means "N audited repos" rather than "N before
// filtering" -- on an account that is mostly archived, the latter would quietly
// audit a handful of repos when asked for fifty.
func (inv *Inventory) InScope(includeForks, includeArchived bool, limit int) Scope {
	scope := Scope{Repos: make([]Repo, 0, len(inv.Repos))}
	for _, r := range inv.Repos {
		if r.IsFork {
			scope.Forks++
			if !includeForks {
				continue
			}
		}
		if r.IsArchived {
			scope.Archived++
			if !includeArchived {
				scope.ArchivedSkipped = append(scope.ArchivedSkipped, r.Name)
				continue
			}
		}
		if limit > 0 && len(scope.Repos) >= limit {
			continue
		}
		scope.Repos = append(scope.Repos, r)
	}
	return scope
}
