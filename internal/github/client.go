package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/ledger"
)

const (
	restRoutingThreshold = 20
	graphqlBatchSize     = 50
)

type MetaState string

const (
	MetaOK          MetaState = "ok"
	MetaNotModified MetaState = "not_modified"
	MetaMissing     MetaState = "missing"
	MetaError       MetaState = "error"
)

// FatalAPIError indicates an unrecoverable auth, rate-limit, or duplicate-ID condition (exit code 1).
type FatalAPIError struct {
	Message string
}

func (e *FatalAPIError) Error() string {
	return e.Message
}

// RepoMeta holds the resolved remote state for a single whitelisted repository.
type RepoMeta struct {
	Target   config.ResolvedTarget
	State    MetaState
	ID       int64
	FullName string
	CloneURL string
	PushedAt time.Time
	ETag     string
	IsEmpty  bool
	Renamed  bool
	Err      error
}

// Client queries GitHub REST and GraphQL APIs for whitelisted repositories.
type Client struct {
	apiBaseURL    string
	graphqlURL    string
	httpClient    *http.Client
	tokenProvider *auth.TokenProvider
	redactor      *auth.Redactor
	sleepFunc     func(context.Context, time.Duration) error
}

func NewClient(tp *auth.TokenProvider, proxyURL string, redactor *auth.Redactor) *Client {
	return &Client{
		apiBaseURL:    auth.DefaultGitHubAPIURL,
		graphqlURL:    auth.DefaultGitHubAPIURL + "/graphql",
		httpClient:    auth.HTTPClient(proxyURL, 30*time.Second),
		tokenProvider: tp,
		redactor:      redactor,
		sleepFunc:     sleepWithContext,
	}
}

// SetEndpoints overrides the REST and GraphQL base URLs (used in tests).
func (c *Client) SetEndpoints(restBaseURL, graphqlURL string) {
	c.apiBaseURL = strings.TrimRight(restBaseURL, "/")
	c.graphqlURL = graphqlURL
}

// ValidateAuth performs a lightweight token check when needed.
func (c *Client) ValidateAuth(ctx context.Context) error {
	_, err := c.tokenProvider.Token(ctx)
	return err
}

// FetchMetadata queries metadata for all targets using REST (<=20) or batched GraphQL (>20).
func (c *Client) FetchMetadata(
	ctx context.Context,
	targets []config.ResolvedTarget,
	existingLedger *ledger.RepoLedger,
	concurrency int,
	maxRetries int,
) ([]RepoMeta, error) {
	var results []RepoMeta
	var err error
	if len(targets) <= restRoutingThreshold {
		results, err = c.fetchViaREST(ctx, targets, existingLedger, concurrency, maxRetries)
	} else {
		results, err = c.fetchViaGraphQL(ctx, targets, maxRetries)
	}
	if err != nil {
		return nil, err
	}

	// Enforce uniqueness by immutable repo_id across all resolved targets.
	seenIDs := make(map[int64]string, len(results))
	for _, r := range results {
		if r.ID <= 0 {
			continue
		}
		if prevRepo, dup := seenIDs[r.ID]; dup {
			return nil, &FatalAPIError{
				Message: fmt.Sprintf("duplicate repository mapping detected: %q and %q both resolve to repo_id %d", prevRepo, r.Target.Repo, r.ID),
			}
		}
		seenIDs[r.ID] = r.Target.Repo
	}
	return results, nil
}

func (c *Client) fetchViaREST(
	ctx context.Context,
	targets []config.ResolvedTarget,
	existingLedger *ledger.RepoLedger,
	concurrency int,
	maxRetries int,
) ([]RepoMeta, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	results := make([]RepoMeta, len(targets))
	jobs := make(chan int, len(targets))
	for i := range targets {
		jobs <- i
	}
	close(jobs)

	var (
		wg       sync.WaitGroup
		fatalMu  sync.Mutex
		fatalErr error
	)

	workers := concurrency
	if workers > len(targets) {
		workers = len(targets)
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				var cachedETag string
				var cachedRec *ledger.RepoRecord
				if existingLedger != nil {
					cachedRec = existingLedger.FindByFullName(targets[idx].Repo)
					if cachedRec != nil && cachedRec.RemotePushedAt != nil && cachedRec.ConsecutiveFailures == 0 {
						cachedETag = cachedRec.ETag
					}
				}

				meta, fErr := c.fetchSingleREST(ctx, targets[idx], cachedETag, cachedRec, maxRetries)
				if fErr != nil {
					fatalMu.Lock()
					if fatalErr == nil {
						fatalErr = fErr
					}
					fatalMu.Unlock()
					return
				}
				results[idx] = meta
			}
		}()
	}
	wg.Wait()

	if fatalErr != nil {
		return nil, fatalErr
	}
	return results, nil
}

func (c *Client) fetchSingleREST(
	ctx context.Context,
	target config.ResolvedTarget,
	cachedETag string,
	cachedRec *ledger.RepoRecord,
	maxRetries int,
) (RepoMeta, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s", c.apiBaseURL, target.Owner, target.Name)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		tok, err := c.tokenProvider.Token(ctx)
		if err != nil {
			return RepoMeta{}, &FatalAPIError{Message: fmt.Sprintf("obtain auth token: %v", err)}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return RepoMeta{Target: target, State: MetaError, Err: err}, nil
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if cachedETag != "" {
			req.Header.Set("If-None-Match", cachedETag)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt < maxRetries {
				if sleepErr := c.sleepFunc(ctx, backoffDuration(attempt+1)); sleepErr != nil {
					return RepoMeta{Target: target, State: MetaError, Err: sleepErr}, nil
				}
				continue
			}
			return RepoMeta{Target: target, State: MetaError, Err: fmt.Errorf("request %s: %s", target.Repo, c.redactErr(err))}, nil
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			if readErr != nil {
				return RepoMeta{Target: target, State: MetaError, Err: readErr}, nil
			}
			var payload struct {
				ID       int64     `json:"id"`
				FullName string    `json:"full_name"`
				PushedAt time.Time `json:"pushed_at"`
				Size     int64     `json:"size"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return RepoMeta{Target: target, State: MetaError, Err: fmt.Errorf("decode REST response: %w", err)}, nil
			}
			canonical := payload.FullName
			if canonical == "" {
				canonical = target.Repo
			}
			return RepoMeta{
				Target:   target,
				State:    MetaOK,
				ID:       payload.ID,
				FullName: canonical,
				CloneURL: "https://github.com/" + canonical + ".git",
				PushedAt: payload.PushedAt.UTC(),
				ETag:     resp.Header.Get("ETag"),
				IsEmpty:  payload.Size == 0,
				Renamed:  strings.ToLower(canonical) != target.CanonicalKey,
			}, nil

		case http.StatusNotModified:
			if cachedRec == nil {
				// Defensive fallback: retry without ETag if cache entry is missing.
				cachedETag = ""
				continue
			}
			var pushed time.Time
			if cachedRec.RemotePushedAt != nil {
				pushed = *cachedRec.RemotePushedAt
			}
			return RepoMeta{
				Target:   target,
				State:    MetaNotModified,
				ID:       cachedRec.ID,
				FullName: cachedRec.FullName,
				CloneURL: "https://github.com/" + cachedRec.FullName + ".git",
				PushedAt: pushed,
				ETag:     cachedETag,
				Renamed:  strings.ToLower(cachedRec.FullName) != target.CanonicalKey,
			}, nil

		case http.StatusNotFound:
			return RepoMeta{
				Target:   target,
				State:    MetaMissing,
				FullName: target.Repo,
				CloneURL: "https://github.com/" + target.Repo + ".git",
			}, nil

		case http.StatusUnauthorized:
			return RepoMeta{}, &FatalAPIError{
				Message: fmt.Sprintf("GitHub API returned 401 Unauthorized: %s", c.redactor.Redact(string(body))),
			}

		case http.StatusForbidden, http.StatusTooManyRequests:
			waitDur, fatal := classifyRateLimitOrForbidden(resp, c.redactor.Redact(string(body)))
			if fatal != nil {
				return RepoMeta{}, fatal
			}
			if waitDur > 0 && attempt < maxRetries {
				if sleepErr := c.sleepFunc(ctx, waitDur); sleepErr != nil {
					return RepoMeta{Target: target, State: MetaError, Err: sleepErr}, nil
				}
				continue
			}
			return RepoMeta{}, &FatalAPIError{Message: fmt.Sprintf("GitHub API rate limit exceeded (%d)", resp.StatusCode)}

		default:
			if resp.StatusCode >= 500 && attempt < maxRetries {
				if sleepErr := c.sleepFunc(ctx, backoffDuration(attempt+1)); sleepErr != nil {
					return RepoMeta{Target: target, State: MetaError, Err: sleepErr}, nil
				}
				continue
			}
			return RepoMeta{
				Target: target,
				State:  MetaError,
				Err:    fmt.Errorf("GitHub REST API status %d: %s", resp.StatusCode, c.redactor.Redact(string(body))),
			}, nil
		}
	}

	return RepoMeta{Target: target, State: MetaError, Err: errors.New("exhausted REST retries")}, nil
}

func (c *Client) fetchViaGraphQL(
	ctx context.Context,
	targets []config.ResolvedTarget,
	maxRetries int,
) ([]RepoMeta, error) {
	results := make([]RepoMeta, len(targets))
	for start := 0; start < len(targets); start += graphqlBatchSize {
		end := start + graphqlBatchSize
		if end > len(targets) {
			end = len(targets)
		}
		batch := targets[start:end]
		batchMeta, err := c.fetchGraphQLBatch(ctx, batch, maxRetries)
		if err != nil {
			return nil, err
		}
		copy(results[start:end], batchMeta)
	}
	return results, nil
}

func (c *Client) fetchGraphQLBatch(
	ctx context.Context,
	batch []config.ResolvedTarget,
	maxRetries int,
) ([]RepoMeta, error) {
	var varDefs []string
	var fields []string
	variables := make(map[string]string, len(batch)*2)

	for i, t := range batch {
		oVar := fmt.Sprintf("o%d", i)
		nVar := fmt.Sprintf("n%d", i)
		varDefs = append(varDefs, fmt.Sprintf("$%s: String!, $%s: String!", oVar, nVar))
		fields = append(fields, fmt.Sprintf("r%d: repository(owner: $%s, name: $%s) { databaseId nameWithOwner pushedAt isEmpty }", i, oVar, nVar))
		variables[oVar] = t.Owner
		variables[nVar] = t.Name
	}

	query := fmt.Sprintf("query(%s) { %s }", strings.Join(varDefs, ", "), strings.Join(fields, " "))
	reqBody, err := json.Marshal(struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}{
		Query:     query,
		Variables: variables,
	})
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		tok, err := c.tokenProvider.Token(ctx)
		if err != nil {
			return nil, &FatalAPIError{Message: fmt.Sprintf("obtain auth token: %v", err)}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphqlURL, bytes.NewReader(reqBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github+json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt < maxRetries {
				if sleepErr := c.sleepFunc(ctx, backoffDuration(attempt+1)); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return c.markBatchError(batch, fmt.Errorf("graphql request failed: %s", c.redactErr(err))), nil
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized {
			return nil, &FatalAPIError{Message: fmt.Sprintf("GitHub GraphQL 401 Unauthorized: %s", c.redactor.Redact(string(body)))}
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			waitDur, fatal := classifyRateLimitOrForbidden(resp, c.redactor.Redact(string(body)))
			if fatal != nil {
				return nil, fatal
			}
			if waitDur > 0 && attempt < maxRetries {
				if sleepErr := c.sleepFunc(ctx, waitDur); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return nil, &FatalAPIError{Message: fmt.Sprintf("GitHub GraphQL rate limit exceeded (%d)", resp.StatusCode)}
		}
		if resp.StatusCode >= 500 {
			if attempt < maxRetries {
				if sleepErr := c.sleepFunc(ctx, backoffDuration(attempt+1)); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return c.markBatchError(batch, fmt.Errorf("GitHub GraphQL status %d: %s", resp.StatusCode, c.redactor.Redact(string(body)))), nil
		}
		if resp.StatusCode != http.StatusOK {
			return c.markBatchError(batch, fmt.Errorf("GitHub GraphQL unexpected status %d: %s", resp.StatusCode, c.redactor.Redact(string(body)))), nil
		}
		if readErr != nil {
			return c.markBatchError(batch, readErr), nil
		}

		return c.parseGraphQLResponse(batch, body)
	}

	return c.markBatchError(batch, errors.New("exhausted GraphQL retries")), nil
}

func (c *Client) parseGraphQLResponse(batch []config.ResolvedTarget, body []byte) ([]RepoMeta, error) {
	var parsed struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Type    string   `json:"type"`
			Message string   `json:"message"`
			Path    []string `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return c.markBatchError(batch, fmt.Errorf("decode GraphQL JSON: %w", err)), nil
	}

	errByAlias := make(map[string]struct {
		typ string
		msg string
	})
	for _, ge := range parsed.Errors {
		if strings.EqualFold(ge.Type, "RATE_LIMITED") {
			return nil, &FatalAPIError{Message: "GitHub GraphQL API rate limit exceeded: " + ge.Message}
		}
		if len(ge.Path) > 0 {
			errByAlias[ge.Path[0]] = struct {
				typ string
				msg string
			}{typ: ge.Type, msg: ge.Message}
		}
	}

	out := make([]RepoMeta, len(batch))
	for i, t := range batch {
		alias := fmt.Sprintf("r%d", i)
		if ge, hasErr := errByAlias[alias]; hasErr {
			if strings.EqualFold(ge.typ, "NOT_FOUND") {
				out[i] = RepoMeta{
					Target:   t,
					State:    MetaMissing,
					FullName: t.Repo,
					CloneURL: "https://github.com/" + t.Repo + ".git",
				}
			} else {
				out[i] = RepoMeta{
					Target: t,
					State:  MetaError,
					Err:    fmt.Errorf("graphql %s: %s", ge.typ, ge.msg),
				}
			}
			continue
		}

		rawNode, ok := parsed.Data[alias]
		if !ok || len(rawNode) == 0 || string(rawNode) == "null" {
			out[i] = RepoMeta{
				Target: t,
				State:  MetaError,
				Err:    errors.New("graphql returned null node without NOT_FOUND error"),
			}
			continue
		}

		var node struct {
			DatabaseID    int64     `json:"databaseId"`
			NameWithOwner string    `json:"nameWithOwner"`
			PushedAt      time.Time `json:"pushedAt"`
			IsEmpty       bool      `json:"isEmpty"`
		}
		if err := json.Unmarshal(rawNode, &node); err != nil {
			out[i] = RepoMeta{Target: t, State: MetaError, Err: err}
			continue
		}
		canonical := node.NameWithOwner
		if canonical == "" {
			canonical = t.Repo
		}
		out[i] = RepoMeta{
			Target:   t,
			State:    MetaOK,
			ID:       node.DatabaseID,
			FullName: canonical,
			CloneURL: "https://github.com/" + canonical + ".git",
			PushedAt: node.PushedAt.UTC(),
			IsEmpty:  node.IsEmpty,
			Renamed:  strings.ToLower(canonical) != t.CanonicalKey,
		}
	}
	return out, nil
}

func (c *Client) markBatchError(batch []config.ResolvedTarget, err error) []RepoMeta {
	out := make([]RepoMeta, len(batch))
	for i, t := range batch {
		out[i] = RepoMeta{Target: t, State: MetaError, Err: err}
	}
	return out
}

func classifyRateLimitOrForbidden(resp *http.Response, body string) (time.Duration, error) {
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			if secs <= 60 {
				if secs < 1 {
					secs = 1
				}
				return time.Duration(secs) * time.Second, nil
			}
			return 0, &FatalAPIError{
				Message: fmt.Sprintf("GitHub API secondary rate limit Retry-After=%ds (>60s), aborting", secs),
			}
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		resetInfo := "unknown"
		if resetStr := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); resetStr != "" {
			if epoch, err := strconv.ParseInt(resetStr, 10, 64); err == nil {
				resetInfo = time.Unix(epoch, 0).UTC().Format(time.RFC3339)
			}
		}
		return 0, &FatalAPIError{
			Message: fmt.Sprintf("GitHub API primary rate limit exhausted (resets at %s)", resetInfo),
		}
	}
	return 0, &FatalAPIError{
		Message: fmt.Sprintf("GitHub API returned %d Forbidden/RateLimited: %s", resp.StatusCode, body),
	}
}

func (c *Client) redactErr(err error) string {
	if err == nil {
		return ""
	}
	return c.redactor.Redact(err.Error())
}

func backoffDuration(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := 2 * time.Second * (1 << (attempt - 1))
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	jitter := time.Duration(rand.IntN(1000)) * time.Millisecond
	return base + jitter
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
