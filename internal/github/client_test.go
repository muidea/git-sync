package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/muidea/git-backup/internal/auth"
	"github.com/muidea/git-backup/internal/config"
	"github.com/muidea/git-backup/internal/ledger"
)

func TestFetchViaRESTWithETagRenameAndMissing(t *testing.T) {
	t.Setenv("GH_TOK", "pat_123")
	redactor := auth.NewRedactor()
	tp, err := auth.NewTokenProvider(config.AuthConfig{Mode: "pat", TokenEnv: "GH_TOK"}, "", redactor)
	if err != nil {
		t.Fatal(err)
	}

	pushed := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/muidea/cached":
			if r.Header.Get("If-None-Match") == `"etag-1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/repos/muidea/oldname":
			w.Header().Set("ETag", `"etag-2"`)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        202,
				"full_name": "muidea/NewName",
				"pushed_at": pushed,
				"size":      128,
			})
		case "/repos/muidea/gone":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	client := NewClient(tp, "", redactor)
	client.SetEndpoints(srv.URL, srv.URL+"/graphql")

	l := ledger.NewEmptyLedger()
	l.Repositories["101"] = &ledger.RepoRecord{
		ID:             101,
		FullName:       "muidea/cached",
		ETag:           `"etag-1"`,
		RemotePushedAt: &pushed,
	}

	targets := []config.ResolvedTarget{
		{Repo: "muidea/cached", Owner: "muidea", Name: "cached", CanonicalKey: "muidea/cached"},
		{Repo: "muidea/oldname", Owner: "muidea", Name: "oldname", CanonicalKey: "muidea/oldname"},
		{Repo: "muidea/gone", Owner: "muidea", Name: "gone", CanonicalKey: "muidea/gone"},
	}

	metas, err := client.FetchMetadata(context.Background(), targets, l, 2, 0)
	if err != nil {
		t.Fatalf("FetchMetadata failed: %v", err)
	}
	if metas[0].State != MetaNotModified || metas[0].ID != 101 {
		t.Fatalf("expected cached 304, got %+v", metas[0])
	}
	if metas[1].State != MetaOK || !metas[1].Renamed || metas[1].FullName != "muidea/NewName" {
		t.Fatalf("expected rename detection, got %+v", metas[1])
	}
	if metas[2].State != MetaMissing {
		t.Fatalf("expected missing state, got %+v", metas[2])
	}
}

func TestFetchViaGraphQLPartialErrorsAndDuplicateRepoID(t *testing.T) {
	t.Setenv("GH_TOK", "pat_123")
	redactor := auth.NewRedactor()
	tp, _ := auth.NewTokenProvider(config.AuthConfig{Mode: "pat", TokenEnv: "GH_TOK"}, "", redactor)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		data := map[string]any{}
		for i := 0; i < 21; i++ {
			alias := fmt.Sprintf("r%d", i)
			switch i {
			case 1:
				data[alias] = nil
			case 2:
				data[alias] = nil
			default:
				data[alias] = map[string]any{
					"databaseId":    1000 + i,
					"nameWithOwner": fmt.Sprintf("org/repo-%d", i),
					"pushedAt":      "2026-11-18T00:00:00Z",
					"isEmpty":       false,
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": data,
			"errors": []map[string]any{
				{"type": "NOT_FOUND", "message": "not found", "path": []string{"r1"}},
				{"type": "FORBIDDEN", "message": "forbidden", "path": []string{"r2"}},
			},
		})
	}))
	defer srv.Close()

	client := NewClient(tp, "", redactor)
	client.SetEndpoints(srv.URL, srv.URL+"/graphql")

	targets := make([]config.ResolvedTarget, 21)
	for i := 0; i < 21; i++ {
		name := fmt.Sprintf("repo-%d", i)
		targets[i] = config.ResolvedTarget{Repo: "org/" + name, Owner: "org", Name: name, CanonicalKey: "org/" + name}
	}

	metas, err := client.FetchMetadata(context.Background(), targets, nil, 2, 0)
	if err != nil {
		t.Fatalf("FetchMetadata GraphQL failed: %v", err)
	}
	if metas[1].State != MetaMissing {
		t.Fatalf("expected r1 MetaMissing, got %+v", metas[1])
	}
	if metas[2].State != MetaError {
		t.Fatalf("expected r2 MetaError (FORBIDDEN must NOT be MetaMissing), got %+v", metas[2])
	}

	// Verify duplicate repo_id check
	srvDup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":        999,
			"full_name": "org/canonical",
			"pushed_at": "2026-11-18T00:00:00Z",
			"size":      10,
		})
	}))
	defer srvDup.Close()
	client.SetEndpoints(srvDup.URL, srvDup.URL+"/graphql")

	dupTargets := []config.ResolvedTarget{
		{Repo: "org/old", Owner: "org", Name: "old", CanonicalKey: "org/old"},
		{Repo: "org/new", Owner: "org", Name: "new", CanonicalKey: "org/new"},
	}
	_, err = client.FetchMetadata(context.Background(), dupTargets, nil, 2, 0)
	var fatal *FatalAPIError
	if !errors.As(err, &fatal) {
		t.Fatalf("expected FatalAPIError on duplicate repo_id, got %v", err)
	}
}
