package audit

import (
	"slices"
	"testing"
)

func repoNames(repos []Repo) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.Name)
	}
	return out
}

func testInventory() *Inventory {
	// Ordered as the API returns them: most recently pushed first.
	return &Inventory{Repos: []Repo{
		{Name: "active-1"},
		{Name: "archived-1", IsArchived: true},
		{Name: "fork-1", IsFork: true},
		{Name: "active-2"},
		{Name: "archived-fork", IsFork: true, IsArchived: true},
		{Name: "archived-2", IsArchived: true},
		{Name: "active-3"},
	}}
}

func TestInScopeSkipsArchivedAndForksByDefault(t *testing.T) {
	scope := testInventory().InScope(false, false, 0)

	if want := []string{"active-1", "active-2", "active-3"}; !slices.Equal(repoNames(scope.Repos), want) {
		t.Errorf("scope = %v, want %v", repoNames(scope.Repos), want)
	}
	// Each dropped repo is attributed to exactly ONE reason, so the two counts
	// never double-report the same repo: archived-fork is a fork here, and is not
	// also counted among the archived.
	if scope.Forks != 2 {
		t.Errorf("Forks = %d, want 2", scope.Forks)
	}
	if scope.Archived != 2 {
		t.Errorf("Archived = %d, want 2 (archived-fork counts as a fork, not twice)", scope.Archived)
	}
	// A fork is dropped before the archived check, so it is not double-reported as
	// a skipped archived repo whose PRs need disclosing.
	if want := []string{"archived-1", "archived-2"}; !slices.Equal(scope.ArchivedSkipped, want) {
		t.Errorf("ArchivedSkipped = %v, want %v", scope.ArchivedSkipped, want)
	}
}

func TestInScopeIncludeArchived(t *testing.T) {
	scope := testInventory().InScope(false, true, 0)

	want := []string{"active-1", "archived-1", "active-2", "archived-2", "active-3"}
	if !slices.Equal(repoNames(scope.Repos), want) {
		t.Errorf("scope = %v, want %v", repoNames(scope.Repos), want)
	}
	if len(scope.ArchivedSkipped) != 0 {
		t.Errorf("nothing was skipped, got %v", scope.ArchivedSkipped)
	}
	if scope.Archived != 2 {
		t.Errorf("Archived = %d, want 2 (counted even when audited)", scope.Archived)
	}
}

// With forks in scope an archived fork is simply an archived repo, so it moves
// from the fork count into the archived count rather than being lost.
func TestInScopeArchivedForkAttribution(t *testing.T) {
	scope := testInventory().InScope(true, false, 0)

	if scope.Archived != 3 {
		t.Errorf("Archived = %d, want 3 once forks are in scope", scope.Archived)
	}
	if slices.Contains(repoNames(scope.Repos), "archived-fork") {
		t.Error("an archived fork must still be dropped when archived repos are skipped")
	}
}

func TestInScopeIncludeForks(t *testing.T) {
	scope := testInventory().InScope(true, false, 0)

	want := []string{"active-1", "fork-1", "active-2", "active-3"}
	if !slices.Equal(repoNames(scope.Repos), want) {
		t.Errorf("scope = %v, want %v", repoNames(scope.Repos), want)
	}
}

// limit must mean "N audited repos". Applying it before filtering would audit a
// handful of repos when asked for many, on an account that is mostly archived.
func TestInScopeAppliesLimitAfterFiltering(t *testing.T) {
	scope := testInventory().InScope(false, false, 2)

	if want := []string{"active-1", "active-2"}; !slices.Equal(repoNames(scope.Repos), want) {
		t.Errorf("scope = %v, want %v", repoNames(scope.Repos), want)
	}
	// Exclusion counts stay account-wide, so the disclosure is not truncated along
	// with the slice.
	if scope.Archived != 2 || scope.Forks != 2 {
		t.Errorf("counts = archived %d / forks %d, want 2 / 2", scope.Archived, scope.Forks)
	}
}

func TestInScopeLimitLargerThanInventory(t *testing.T) {
	scope := testInventory().InScope(false, false, 500)
	if len(scope.Repos) != 3 {
		t.Errorf("scope size = %d, want 3", len(scope.Repos))
	}
}
