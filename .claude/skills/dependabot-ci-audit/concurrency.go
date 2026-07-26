package audit

import (
	"context"
	"sync"
)

// mapRepos runs fn over every repo with at most concurrency workers in flight,
// keyed by repo name.
//
// fn must return a state rather than an error: a per-repo failure belongs in
// that repo's row as ERROR, not as an aborted audit. Bounded rather than
// unlimited because these are the per-repo calls, and firing hundreds at once
// trips GitHub's secondary rate limit -- which costs more time than it saves.
func mapRepos[T any](ctx context.Context, repos []Repo, concurrency int, fn func(Repo) T) map[string]T {
	if concurrency < 1 {
		concurrency = 1
	}

	results := make(map[string]T, len(repos))
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, concurrency)

	for _, repo := range repos {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		slots <- struct{}{}
		go func(r Repo) {
			defer wg.Done()
			defer func() { <-slots }()
			value := fn(r)
			mu.Lock()
			results[r.Name] = value
			mu.Unlock()
		}(repo)
	}
	wg.Wait()
	return results
}
