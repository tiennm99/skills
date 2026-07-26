package audit

import _ "embed"

// The GraphQL queries stay in .graphql files rather than Go string literals so
// their load-bearing comments survive. Read them before editing either query.
var (
	//go:embed queries/inventory.graphql
	inventoryQuery string

	//go:embed queries/ci-sweep.graphql
	ciSweepQuery string
)

// DefaultWaiverList is waivers.txt, compiled in.
//
// Embedding rather than resolving a path at runtime is deliberate: `go run`
// gives the process no reliable handle on its own source directory, so a
// relative default would break whenever the tool was invoked from anywhere but
// the skill directory -- and a waiver file that silently fails to load would
// turn accepted blind spots back into findings.
//
// Editing the file takes effect on the next `go run` (the embed invalidates the
// build cache). A prebuilt binary needs a rebuild, or -waiver-file to point at
// the file on disk. The summary always names the repos it waived, so a stale
// compiled-in list is visible in the output rather than silent.
//
//go:embed waivers.txt
var DefaultWaiverList string
