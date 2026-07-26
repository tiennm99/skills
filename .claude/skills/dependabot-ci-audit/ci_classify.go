package audit

import (
	"strings"
)

// CIState is the verdict on a repo's LATEST COMMIT only.
//
// Historical run failures are noise: a repo whose HEAD is green is healthy
// regardless of what failed months ago. Widening to a run-history window
// inflates failure counts by roughly an order of magnitude.
type CIState string

const (
	CIGreen         CIState = "GREEN"
	CINoCI          CIState = "NO_CI"
	CINoCommits     CIState = "NO_COMMITS"
	CIBuildFailed   CIState = "BUILD_FAILED"
	CIDependabotJob CIState = "DEPENDABOT_JOB_FAILED"
	CIStuck         CIState = "STUCK"
	CIError         CIState = "ERROR"
)

// updaterCheckName is Dependabot's own updater check-run, matched EXACTLY.
//
// It is not application CI: it routinely fails with
// security_update_not_possible while the project's build and tests pass on the
// very same commit. Do not broaden this to a "Dependabot / *" prefix -- that
// would swallow a user workflow named Dependabot and understate real breakage,
// an error in the dangerous direction.
const updaterCheckName = "Dependabot"

// Check is one check-run or commit status on a commit, from either API path.
type Check struct {
	Name  string
	State string
}

// failedStates are terminal failures.
//
// "error" matters as much as "failure": commit statuses from Vercel and
// Cloudflare report a broken deploy as `error`, and omitting it classifies
// those repos GREEN.
var failedStates = map[string]bool{
	"failure":         true,
	"error":           true,
	"timed_out":       true,
	"startup_failure": true,
	"action_required": true,
}

// unsettledStates never reached a terminal state. "cancelled" is here rather
// than in failedStates on purpose: a cancelled run is an absent answer, not a
// failing one.
var unsettledStates = map[string]bool{
	"pending":     true,
	"queued":      true,
	"in_progress": true,
	"waiting":     true,
	"requested":   true,
	"expected":    true,
	"cancelled":   true,
}

// NormalizeCheck fills in the blanks a raw API response can carry: an in-flight
// check-run has no conclusion, only a status, and either field can be absent.
//
// An unknown state is deliberately NOT treated as a failure or as settled, so
// it lands in neither vocabulary and leaves the repo GREEN only when nothing
// else is wrong. Silently promoting "unknown" to "failure" would manufacture
// breakage; the detail column still shows it.
func NormalizeCheck(name, conclusion, status string) Check {
	state := conclusion
	if state == "" {
		state = status
	}
	if state == "" {
		state = "unknown"
	}
	if name == "" {
		name = "?"
	}
	return Check{Name: name, State: strings.ToLower(state)}
}

// Classify turns a commit's checks into a state and a detail string.
//
// hasCommits distinguishes an empty repo (nothing to build or expose, never a
// finding) from a repo with commits and no CI configured. checks must arrive
// with commit statuses FIRST, then check-runs, so the detail column reads in a
// stable order regardless of which API path produced it.
//
// Precedence is load-bearing: the project's own failure outranks an unsettled
// check, which outranks the Dependabot updater's failure. Conflating the first
// and last massively overstates breakage.
func Classify(hasCommits bool, checks []Check) (CIState, string) {
	if !hasCommits {
		return CINoCommits, ""
	}

	details := make([]string, 0, len(checks))
	var appFailed, appUnsettled, updaterFailed int
	for _, c := range checks {
		details = append(details, c.Name+"="+c.State)
		switch {
		case c.Name == updaterCheckName:
			if failedStates[c.State] {
				updaterFailed++
			}
		case failedStates[c.State]:
			appFailed++
		case unsettledStates[c.State]:
			appUnsettled++
		}
	}
	detail := strings.Join(details, ", ")

	switch {
	case len(checks) == 0:
		return CINoCI, ""
	case appFailed > 0:
		return CIBuildFailed, detail
	case appUnsettled > 0:
		return CIStuck, detail
	case updaterFailed > 0:
		return CIDependabotJob, detail
	default:
		return CIGreen, detail
	}
}
