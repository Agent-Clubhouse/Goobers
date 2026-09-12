package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

type inventoryLabels struct {
	present map[string]bool
	reads   []string
	writes  []string
	fail    string
}

func (l *inventoryLabels) ReadClaimed(_ context.Context, key string) (bool, error) {
	l.reads = append(l.reads, key)
	return l.present[key], nil
}

func TestSharedInventoryRejectsInvalidInputBeforeLabelIO(t *testing.T) {
	for _, body := range []string{"null", "{}", "[", `[{"ref":"refs/heads/main"}]`, strings.Repeat(" ", 4<<20+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/acme/repo/git/matching-refs/heads/goobers-shared-claims/" {
				t.Errorf("unexpected inventory path: %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(body))
		}))
		store := GitHubSharedClaimStore{Provider: NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL; p.maxRetries = 0 }), Repository: RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "repo"}}
		labels := &inventoryLabels{present: map[string]bool{}}
		_, err := store.ReconcileSharedVisibility(t.Context(), labels, "", 1)
		server.Close()
		if err == nil || len(labels.reads) != 0 || len(labels.writes) != 0 {
			t.Fatal("invalid inventory reached labels")
		}
	}
	store := GitHubSharedClaimStore{}
	labels := &inventoryLabels{present: map[string]bool{}}
	for _, cursor := range []string{"https://untrusted.invalid/", "refs/heads/main", sharedRefPrefix + strings.Repeat("A", 64)} {
		if _, err := store.ReconcileSharedVisibility(t.Context(), labels, cursor, 1); err == nil {
			t.Fatalf("invalid cursor accepted: %s", cursor)
		}
	}
}
func (l *inventoryLabels) SetClaimed(_ context.Context, key string, present bool) error {
	l.writes = append(l.writes, key)
	if key == l.fail {
		return errors.New("issue unavailable")
	}
	l.present[key] = present
	return nil
}

func inventoryStore(t *testing.T, records map[string]sharedclaim.Record, corrupt ...string) GitHubSharedClaimStore {
	t.Helper()
	var refs []sharedGitRef
	commits := make(map[string]sharedGitCommit)
	byRef := make(map[string]sharedGitRef)
	for key, record := range records {
		data, err := sharedclaim.Encode(key, record)
		if err != nil {
			t.Fatal(err)
		}
		for _, broken := range corrupt {
			if key == broken {
				data = []byte(`{"protocol":"unknown","key":"another-item"}`)
			}
		}
		var ref sharedGitRef
		ref.Ref, ref.Object.Type, ref.Object.SHA = sharedClaimRef(key), "commit", fmt.Sprintf("%040x", len(refs)+1)
		commit := sharedGitCommit{SHA: ref.Object.SHA, Message: string(data)}
		commit.Tree.SHA = strings.Repeat("f", 40)
		refs = append(refs, ref)
		commits[commit.SHA], byRef[ref.Ref] = commit, ref
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer contents-token" {
			t.Errorf("inventory changed authority or used wrong credentials: %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/git/")
		switch {
		case strings.TrimSuffix(path, "/") == "matching-refs/heads/goobers-shared-claims":
			_ = json.NewEncoder(w).Encode(refs)
		case strings.HasPrefix(path, "commits/"):
			commit, ok := commits[strings.TrimPrefix(path, "commits/")]
			if !ok {
				t.Error("read unknown commit")
			}
			_ = json.NewEncoder(w).Encode(commit)
		case strings.HasPrefix(path, "ref/"):
			ref, ok := byRef["refs/"+strings.TrimPrefix(path, "ref/")]
			if !ok {
				t.Error("read unknown ref")
			}
			_ = json.NewEncoder(w).Encode(ref)
		default:
			t.Errorf("unexpected inventory path: %s", path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	return GitHubSharedClaimStore{Provider: NewGitHubProvider("contents-token", func(p *GitHubProvider) { p.BaseURL = server.URL; p.maxRetries = 0 }),
		Repository: RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "repo"}}
}

func TestSharedInventoryMalformedRecordDoesNotStarveValidRecord(t *testing.T) {
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}
	record := sharedclaim.Record{Version: 1, Owner: owner, ExpiresAt: time.Now().Add(time.Hour)}
	store := inventoryStore(t, map[string]sharedclaim.Record{"42": record, "43": record}, "42")
	labels := &inventoryLabels{present: map[string]bool{}}
	if cursor, err := store.ReconcileSharedVisibility(t.Context(), labels, "", 128); err == nil || cursor != "" || !labels.present["43"] {
		t.Fatalf("bad record starved valid record: %q %v %+v", cursor, err, labels.present)
	}
	for _, key := range labels.reads {
		if key != "43" {
			t.Fatalf("unvalidated record reached issue API: %q", key)
		}
	}
}

func TestSharedInventoryRepairsOrphanedAndExpiredLabelsWithoutLedger(t *testing.T) {
	owner := sharedclaim.Owner{Instance: "crashed", Run: "run", Token: "token"}
	store := inventoryStore(t, map[string]sharedclaim.Record{
		"42":    {Version: 1, Owner: owner, ExpiresAt: time.Now().Add(time.Hour)},
		"43":    {Version: 1, Owner: owner, ExpiresAt: time.Now().Add(-time.Hour)},
		"44":    {Version: 1},
		"pr/45": {Version: 1, Owner: owner, ExpiresAt: time.Now().Add(time.Hour)},
	})
	labels := &inventoryLabels{present: map[string]bool{"43": true, "44": true, "45": true}}
	cursor := ""
	for pass := 0; pass < 4; pass++ {
		next, err := store.ReconcileSharedVisibility(t.Context(), labels, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if pass < 3 && (next == "" || next == cursor) {
			t.Fatal("bounded inventory did not advance")
		}
		cursor = next
	}
	if cursor != "" || !labels.present["42"] || labels.present["43"] || labels.present["44"] || !labels.present["45"] || len(labels.writes) != 3 {
		t.Fatalf("incorrect orphan repair: %q %+v %+v", cursor, labels.present, labels.writes)
	}
}

func TestSharedInventoryFailureDoesNotStarveOtherItemsAndRetries(t *testing.T) {
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}
	store := inventoryStore(t, map[string]sharedclaim.Record{
		"42": {Version: 1, Owner: owner, ExpiresAt: time.Now().Add(time.Hour)},
		"43": {Version: 1, Owner: owner, ExpiresAt: time.Now().Add(time.Hour)},
	})
	labels := &inventoryLabels{present: map[string]bool{}, fail: "42"}
	if cursor, err := store.ReconcileSharedVisibility(t.Context(), labels, "", 128); err == nil || cursor != "" || !labels.present["43"] || labels.present["42"] {
		t.Fatalf("failed issue starved successor: %q %v %+v", cursor, err, labels.present)
	}
	labels.fail = ""
	if _, err := store.ReconcileSharedVisibility(t.Context(), labels, "", 128); err != nil || !labels.present["42"] {
		t.Fatalf("retry: %v", err)
	}
}
