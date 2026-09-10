package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

// GitHubSharedClaimStore stores coordination in a dedicated append-only ref.
// Shared mode requires Contents write permission in addition to issue-label
// permission. Local visibility never constructs or writes this store.
type GitHubSharedClaimStore struct {
	Provider   *GitHubProvider
	Repository RepositoryRef
}

type sharedGitCommit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Tree    struct {
		SHA string `json:"sha"`
	} `json:"tree"`
}

type sharedGitRef struct {
	Ref    string `json:"ref"`
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

func (s GitHubSharedClaimStore) endpoint(parts ...string) (string, error) {
	if s.Provider == nil || s.Repository.Provider != ProviderGitHub || s.Repository.Owner == "" || s.Repository.Name == "" {
		return "", fmt.Errorf("shared claims require a GitHub repository")
	}
	return joinURL(s.Provider.BaseURL, append([]string{"repos", s.Repository.Owner, s.Repository.Name, "git"}, parts...)...)
}

func sharedClaimRef(key string) string {
	digest := sha256.Sum256([]byte(key))
	return sharedRefPrefix + hex.EncodeToString(digest[:])
}

// Read verifies the protocol and item binding before returning a lease. The
// Date header supplies the provider clock; missing clock evidence fails closed.
func (s GitHubSharedClaimStore) Read(ctx context.Context, key string) (sharedclaim.Observation, error) {
	endpoint, err := s.endpoint("ref", strings.TrimPrefix(sharedClaimRef(key), "refs/"))
	if err != nil {
		return sharedclaim.Observation{}, err
	}
	response, err := s.Provider.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return sharedclaim.Observation{}, err
	}
	now, clockErr := http.ParseTime(response.Header.Get("Date"))
	if clockErr != nil {
		_ = response.Body.Close()
		return sharedclaim.Observation{}, fmt.Errorf("shared claim response has no provider clock")
	}
	if response.StatusCode == http.StatusNotFound {
		_ = response.Body.Close()
		return sharedclaim.Observation{Now: now}, nil
	}
	var ref sharedGitRef
	if err := readSharedGitResponse(response, &ref); err != nil {
		return sharedclaim.Observation{}, err
	}
	if ref.Ref != sharedClaimRef(key) || ref.Object.Type != "commit" || !sharedGitSHA(ref.Object.SHA) {
		return sharedclaim.Observation{}, fmt.Errorf("invalid shared claim reference")
	}
	commit, err := s.commit(ctx, ref.Object.SHA)
	if err != nil {
		return sharedclaim.Observation{}, err
	}
	record, err := sharedclaim.Decode(key, []byte(commit.Message))
	if err != nil {
		return sharedclaim.Observation{}, err
	}
	return sharedclaim.Observation{Now: now, Revision: ref.Object.SHA, Record: record}, nil
}

func (s GitHubSharedClaimStore) commit(ctx context.Context, sha string) (sharedGitCommit, error) {
	endpoint, err := s.endpoint("commits", sha)
	if err != nil {
		return sharedGitCommit{}, err
	}
	response, err := s.Provider.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return sharedGitCommit{}, err
	}
	var commit sharedGitCommit
	if err := readSharedGitResponse(response, &commit); err != nil {
		return sharedGitCommit{}, err
	}
	if commit.SHA != sha || !sharedGitSHA(commit.Tree.SHA) {
		return sharedGitCommit{}, fmt.Errorf("invalid shared claim commit")
	}
	return commit, nil
}

// CompareAndSwap creates a child of the observed revision, then performs a
// non-forced ref update. Competing children cannot both fast-forward the ref.
// Releases are child commits too: no force-update or ref deletion is used.
func (s GitHubSharedClaimStore) CompareAndSwap(ctx context.Context, key, revision string, record sharedclaim.Record) error {
	data, err := sharedclaim.Encode(key, record)
	if err != nil {
		return err
	}
	if revision != "" && !sharedGitSHA(revision) {
		return fmt.Errorf("invalid shared claim revision")
	}
	var tree string
	parents := []string{}
	if revision != "" {
		previous, err := s.commit(ctx, revision)
		if err != nil {
			return err
		}
		if _, err := sharedclaim.Decode(key, []byte(previous.Message)); err != nil {
			return err
		}
		tree, parents = previous.Tree.SHA, []string{revision}
	} else {
		var created struct {
			SHA string `json:"sha"`
		}
		err := s.write(ctx, "trees", http.MethodPost, map[string]any{"tree": []map[string]string{{
			"path": "shared-claim-protocol", "mode": "100644", "type": "blob", "content": "goobers/shared-claim/v1\n",
		}}}, &created)
		if err != nil {
			return err
		}
		tree = created.SHA
	}
	if !sharedGitSHA(tree) {
		return fmt.Errorf("invalid shared claim tree")
	}
	var created sharedGitCommit
	if err := s.write(ctx, "commits", http.MethodPost, map[string]any{"message": string(data), "tree": tree, "parents": parents}, &created); err != nil {
		return err
	}
	if !sharedGitSHA(created.SHA) {
		return fmt.Errorf("invalid new shared claim commit")
	}
	return s.updateReference(ctx, key, revision, created.SHA, record.ExpiresAt)
}

func (s GitHubSharedClaimStore) updateReference(ctx context.Context, key, revision, sha string, expires time.Time) error {
	var confirmed sharedGitRef
	var err error
	if revision == "" {
		err = s.writeUntil(ctx, "refs", http.MethodPost, map[string]any{"ref": sharedClaimRef(key), "sha": sha}, &confirmed, expires)
	} else {
		err = s.writeUntil(ctx, "refs/"+strings.TrimPrefix(sharedClaimRef(key), "refs/"), http.MethodPatch,
			map[string]any{"sha": sha, "force": false}, &confirmed, expires)
	}
	if err != nil {
		return err
	}
	if confirmed.Ref != sharedClaimRef(key) || confirmed.Object.Type != "commit" || confirmed.Object.SHA != sha {
		return fmt.Errorf("shared claim update acknowledgment does not match")
	}
	return nil
}

func (s GitHubSharedClaimStore) write(ctx context.Context, path, method string, body, out any) error {
	return s.writeUntil(ctx, path, method, body, out, time.Time{})
}

func (s GitHubSharedClaimStore) writeUntil(ctx context.Context, path, method string, body, out any, expires time.Time) error {
	endpoint, err := s.endpoint(path)
	if err != nil {
		return err
	}
	response, err := s.Provider.send(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusUnprocessableEntity {
		_ = response.Body.Close()
		return sharedclaim.ErrConflict
	}
	expected := http.StatusOK
	if method == http.MethodPost {
		expected = http.StatusCreated
	}
	if response.StatusCode != expected {
		_ = response.Body.Close()
		return fmt.Errorf("shared claim write not confirmed (HTTP %d)", response.StatusCode)
	}
	if !expires.IsZero() {
		now, err := http.ParseTime(response.Header.Get("Date"))
		if err != nil || !now.Before(expires) {
			_ = response.Body.Close()
			return fmt.Errorf("shared claim acknowledgment has no live lease")
		}
	}
	return readSharedGitResponse(response, out)
}

func readSharedGitResponse(response *http.Response, out any) error {
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("shared claim GitHub request refused (HTTP %d)", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil {
		return err
	}
	if len(data) > 64<<10 {
		return fmt.Errorf("shared claim GitHub response exceeds bound")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func sharedGitSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}
