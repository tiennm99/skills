package audit

import "testing"

// checks builds a check list from name/state pairs.
func checks(pairs ...string) []Check {
	var out []Check
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, NormalizeCheck(pairs[i], pairs[i+1], ""))
	}
	return out
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name       string
		hasCommits bool
		checks     []Check
		want       CIState
	}{
		{
			name:       "empty repo is never a finding",
			hasCommits: false,
			want:       CINoCommits,
		},
		{
			name:       "commits but no checks configured",
			hasCommits: true,
			want:       CINoCI,
		},
		{
			name:       "all green",
			hasCommits: true,
			checks:     checks("build", "success", "test", "success"),
			want:       CIGreen,
		},
		{
			// The whole reason the updater is classified separately: it fails for
			// dependency reasons while the build passes on the same commit.
			name:       "only the updater failed, app CI green",
			hasCommits: true,
			checks:     checks("Dependabot", "failure", "build", "success"),
			want:       CIDependabotJob,
		},
		{
			// Precedence: real breakage must not be reported as a mere updater
			// failure just because both are present.
			name:       "app failure outranks an updater failure",
			hasCommits: true,
			checks:     checks("Dependabot", "failure", "build", "failure"),
			want:       CIBuildFailed,
		},
		{
			name:       "unsettled outranks an updater failure",
			hasCommits: true,
			checks:     checks("Dependabot", "failure", "deploy", "pending"),
			want:       CIStuck,
		},
		{
			// A workflow the user named "Dependabot / config" is THEIR CI, not the
			// updater. Matching on a prefix would understate real breakage.
			name:       "a user workflow whose name starts with Dependabot is app CI",
			hasCommits: true,
			checks:     checks("Dependabot / config", "failure"),
			want:       CIBuildFailed,
		},
		{
			// Vercel and Cloudflare report a broken deploy as `error`, not
			// `failure`; omitting it would classify this GREEN.
			name:       "a third-party status in error state is a build failure",
			hasCommits: true,
			checks:     checks("vercel", "error"),
			want:       CIBuildFailed,
		},
		{
			name:       "timed out counts as failed",
			hasCommits: true,
			checks:     checks("build", "timed_out"),
			want:       CIBuildFailed,
		},
		{
			name:       "cancelled is unsettled, not failed",
			hasCommits: true,
			checks:     checks("build", "cancelled"),
			want:       CIStuck,
		},
		{
			// Common on archived repos: a status that will never resolve.
			name:       "a pending status that never resolves",
			hasCommits: true,
			checks:     checks("vercel", "pending", "build", "success"),
			want:       CIStuck,
		},
		{
			// An unrecognized state must not be promoted to a failure -- that
			// would manufacture breakage. It still shows in the detail column.
			name:       "an unknown state does not invent a failure",
			hasCommits: true,
			checks:     checks("mystery", "who_knows"),
			want:       CIGreen,
		},
		{
			name:       "state matching is case-insensitive",
			hasCommits: true,
			checks:     checks("build", "FAILURE"),
			want:       CIBuildFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Classify(tc.hasCommits, tc.checks)
			if got != tc.want {
				t.Errorf("Classify() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestNormalizeCheckFallsBackToStatus(t *testing.T) {
	// An in-flight check-run has no conclusion yet, only a status. Reading just
	// the conclusion would make it look settled.
	got := NormalizeCheck("build", "", "in_progress")
	if got.State != "in_progress" {
		t.Errorf("state = %q, want in_progress", got.State)
	}
	if _, ok := unsettledStates[got.State]; !ok {
		t.Error("an in-flight run must classify as unsettled")
	}
}

func TestClassifyDetailOrderIsStable(t *testing.T) {
	// The detail column is compared between the two CI paths, so its order has to
	// come from the input rather than from map iteration.
	_, detail := Classify(true, checks("vercel", "success", "build", "failure"))
	if want := "vercel=success, build=failure"; detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
}
