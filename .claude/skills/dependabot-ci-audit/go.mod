// Local module, never fetched: the import path has no domain on purpose.
//
// go-github supplies the typed REST surface (Dependabot alerts, PR search,
// check-runs, commit statuses) and its typed *ErrorResponse is what lets the
// alerts classifier tell a 403-archived from a 403-disabled without grepping a
// response body. It has no GraphQL support, so the two batched queries are
// POSTed verbatim through the same client -- see queries/ci-sweep.graphql for
// why those files stay hand-written.
module dependabot-ci-audit

go 1.25.0

require github.com/google/go-github/v89 v89.0.0

require github.com/google/go-querystring v1.2.0 // indirect
