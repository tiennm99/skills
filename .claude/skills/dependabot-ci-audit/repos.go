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

// Inventory is the audit's scope: every repo the owner owns, most recently
// pushed first, forks included so the caller can count and then drop them.
type Inventory struct {
	Repos []Repo
	Forks int
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

// FetchInventory pages through the owner's repos, stopping at limit.
//
// limit slices by push date (see queries/inventory.graphql), so it means "the N
// most recently touched repos" and is applied BEFORE forks are filtered out --
// a fork entering the slice therefore makes the audited scope smaller than the
// limit, which is correct rather than a shortfall.
func FetchInventory(ctx context.Context, client *ghapi.Client, owner string, limit int) (*Inventory, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("repo limit must be positive, got %d", limit)
	}

	inv := &Inventory{}
	cursor := ""
	prefix := owner + "/"
	for {
		pageSize := min(100, limit-len(inv.Repos))
		if pageSize <= 0 {
			break
		}

		vars := map[string]any{"owner": owner, "pageSize": pageSize}
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
			if r.IsFork {
				inv.Forks++
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

// InScope drops forks unless asked for. Their alerts and Dependabot PRs belong
// to the upstream project, not to this owner.
func (inv *Inventory) InScope(includeForks bool) []Repo {
	if includeForks {
		return inv.Repos
	}
	scope := make([]Repo, 0, len(inv.Repos))
	for _, r := range inv.Repos {
		if !r.IsFork {
			scope = append(scope, r)
		}
	}
	return scope
}
