// Package ghapi wraps go-github with the two things this audit needs and
// go-github does not provide: token discovery via the gh CLI, and a paginated
// GraphQL caller.
//
// The credential is never stored, logged, or printed. It is read from the
// environment or handed over by `gh auth token`, so gh remains the only thing
// that owns it.
package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-github/v89/github"
)

// Client is a go-github client plus GraphQL. Embedding rather than wrapping
// keeps every typed REST method available at the call site.
type Client struct {
	*github.Client
}

// New resolves a token and returns a ready client. Order: GH_TOKEN,
// GITHUB_TOKEN, then `gh auth token`. gh is consulted last so an explicitly
// exported token always wins, which is how CI overrides a developer login.
func New(ctx context.Context) (*Client, error) {
	token, err := resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	client, err := github.NewClient(
		github.WithAuthToken(token),
		github.WithUserAgent("dependabot-ci-audit"),
	)
	if err != nil {
		return nil, fmt.Errorf("building the GitHub client: %w", err)
	}
	return &Client{Client: client}, nil
}

func resolveToken(ctx context.Context) (string, error) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v, nil
		}
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("gh is not authenticated: run `gh auth login`, or export GH_TOKEN")
		}
		return "", fmt.Errorf("no GH_TOKEN/GITHUB_TOKEN set and the gh CLI is unavailable: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("`gh auth token` returned nothing: run `gh auth login`, or export GH_TOKEN")
	}
	return token, nil
}

// Login returns the authenticated user's login, for defaulting the owner.
func (c *Client) Login(ctx context.Context) (string, error) {
	user, _, err := c.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("reading the authenticated user: %w", err)
	}
	return user.GetLogin(), nil
}

// graphQLResponse mirrors the envelope: data and errors can both be present,
// and a partial `data` alongside `errors` must never be treated as a result.
type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"errors"`
}

// GraphQL POSTs a query and decodes `data` into out.
//
// Any error is returned rather than tolerated: a sweep that half-fails would
// classify the missing repos as having no CI, turning "unknown" into "all
// green" -- the single most damaging error this audit can make.
func (c *Client) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	body := struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables,omitempty"`
	}{Query: query, Variables: vars}

	// BaseURL is https://api.github.com/, so the relative "graphql" resolves to
	// the v4 endpoint while keeping go-github's auth and error handling.
	req, err := c.NewRequest(ctx, "POST", "graphql", body)
	if err != nil {
		return fmt.Errorf("building the GraphQL request: %w", err)
	}

	var envelope graphQLResponse
	if _, err := c.Do(req, &envelope); err != nil {
		return fmt.Errorf("GraphQL request failed: %w", err)
	}
	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			if e.Type != "" {
				messages = append(messages, e.Type+": "+e.Message)
				continue
			}
			messages = append(messages, e.Message)
		}
		return fmt.Errorf("GraphQL returned errors: %s", strings.Join(messages, "; "))
	}
	if len(envelope.Data) == 0 {
		return errors.New("GraphQL returned no data")
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decoding the GraphQL response: %w", err)
	}
	return nil
}

// Retry runs fn, waiting out GitHub's primary and secondary rate limits.
//
// go-github types both as distinct errors but does not retry them, and the
// per-repo alert pass issues one call per repo -- exactly the shape that trips
// the secondary limit. Everything else fails immediately: retrying a 404 or a
// 403-disabled would only delay a correct answer.
func Retry(ctx context.Context, attempts int, fn func() error) error {
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		wait, ok := rateLimitWait(err)
		if !ok || attempt == attempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return err
}

// rateLimitWait reports how long to wait before a retry, and whether err is a
// rate limit at all. The cap keeps a run bounded: a primary-limit reset can be
// an hour away, which is a failure to report, not a delay to sit through.
func rateLimitWait(err error) (time.Duration, bool) {
	const maxWait = 90 * time.Second

	clamp := func(d time.Duration) (time.Duration, bool) {
		if d <= 0 {
			return time.Second, true
		}
		if d > maxWait {
			return 0, false
		}
		return d, true
	}

	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		if abuse.RetryAfter != nil {
			return clamp(*abuse.RetryAfter)
		}
		return 5 * time.Second, true
	}
	var limit *github.RateLimitError
	if errors.As(err, &limit) {
		return clamp(time.Until(limit.Rate.Reset.Time))
	}
	return 0, false
}
