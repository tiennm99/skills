package audit

import (
	"bufio"
	"strings"
)

// Waivers is the set of repos whose Dependabot state is disabled on purpose.
//
// A waiver suppresses Dependabot-STATE findings only: the alerts check and
// leftover updater check-run failures. The repo's own build failures, stuck
// third-party statuses and open Dependabot PRs are still reported, so real
// breakage on a waived repo never disappears along with the noise.
//
// A waived repo is UNMEASURED, not clean. The alerts call is skipped, so alerts
// being re-enabled -- and any advisory they then report -- goes unseen. Callers
// must name waived repos in their output instead of folding them into a total.
type Waivers struct {
	set   map[string]bool
	order []string
}

// ParseWaivers reads one repo name per line, with "#" beginning a comment.
//
// CR is stripped because the file is edited on Windows: a trailing \r makes
// "name\r" != "name", which would waive nothing while looking correct.
func ParseWaivers(text string) Waivers {
	w := Waivers{set: map[string]bool{}}
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(strings.ReplaceAll(line, "\r", ""))
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if w.set[name] {
			continue
		}
		w.set[name] = true
		w.order = append(w.order, name)
	}
	return w
}

// Has reports whether the repo's Dependabot findings are waived.
func (w Waivers) Has(name string) bool { return w.set[name] }

// Names returns the configured repos in file order, for the summary.
func (w Waivers) Names() []string { return w.order }

// Len is the number of configured waivers, which is not the number that matched
// any audited repo -- the summary reports both so a waiver for a repo that no
// longer exists is visible.
func (w Waivers) Len() int { return len(w.order) }
