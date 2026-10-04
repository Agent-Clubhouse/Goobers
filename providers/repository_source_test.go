package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func sourceJSON(t *testing.T, value interface{}) *http.Response {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{`W/"transport-only"`}}, ContentLength: -1, Body: io.NopCloser(strings.NewReader(string(raw)))}
}

func TestGitHubRepositorySourceVerifiesPinnedTreeAndBlob(t *testing.T) {
	for _, mode := range []string{"regular", "symlink", "submodule", "truncated", "tamper", "wrong-path", "wrong-commit"} {
		t.Run(mode, func(t *testing.T) {
			commit, root, dir := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
			content := []byte("# Actual source\n")
			blob := sourceBlobID(content)
			blobReads := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || r.URL.RawQuery != "" {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				switch r.URL.Path {
				case "/repos/org/repo/git/ref/heads/main":
					return sourceJSON(t, map[string]interface{}{"ref": "refs/heads/main", "object": map[string]string{"sha": commit}}), nil
				case "/repos/org/repo/git/commits/" + commit:
					observed := commit
					if mode == "wrong-commit" {
						observed = root
					}
					return sourceJSON(t, map[string]interface{}{"sha": observed, "tree": map[string]string{"sha": root}}), nil
				case "/repos/org/repo/git/trees/" + root:
					return sourceJSON(t, map[string]interface{}{"sha": root, "tree": []sourceTreeEntry{{Path: "docs", Mode: "040000", Type: "tree", SHA: dir}}}), nil
				case "/repos/org/repo/git/trees/" + dir:
					entry := sourceTreeEntry{Path: "plan.md", Mode: "100644", Type: "blob", SHA: blob, Size: int64(len(content))}
					if mode == "symlink" {
						entry.Mode = "120000"
					}
					if mode == "submodule" {
						entry.Mode = "160000"
						entry.Type = "commit"
					}
					if mode == "wrong-path" {
						entry.Path = "other.md"
					}
					return sourceJSON(t, map[string]interface{}{"sha": dir, "truncated": mode == "truncated", "tree": []sourceTreeEntry{entry}}), nil
				case "/repos/org/repo/git/blobs/" + blob:
					blobReads++
					served := append([]byte(nil), content...)
					if mode == "tamper" {
						served[0] = '!'
					}
					return sourceJSON(t, map[string]interface{}{"sha": blob, "size": len(served), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(served)}), nil
				default:
					t.Fatalf("followed unconfigured path: %s", r.URL)
					return nil, errors.New("unexpected")
				}
			})}
			p := NewGitHubProvider("", WithHTTPClient(client))
			repo := RepositoryRef{Provider: ProviderGitHub, Owner: "org", Name: "repo"}
			head, err := p.ReadSourceBranch(context.Background(), repo, "main")
			if err != nil || head != commit {
				t.Fatalf("head %q %v", head, err)
			}
			file, err := p.ReadRepositorySource(context.Background(), repo, "docs/plan.md", head)
			if mode == "regular" {
				if err != nil || string(file.Content) != string(content) || file.BlobID != blob || file.ETag != `W/"transport-only"` {
					t.Fatalf("file %+v %v", file, err)
				}
			} else if !errors.Is(err, ErrRepositorySource) {
				t.Fatalf("unsafe file accepted %+v %v", file, err)
			}
			if mode != "regular" && mode != "tamper" && blobReads != 0 {
				t.Fatal("fetched blob despite invalid provenance")
			}
		})
	}
}

func TestADORepositorySourceVerifiesExactNativeIdentity(t *testing.T) {
	for _, mode := range []string{"regular", "symlink", "folder", "submodule", "wrong-path", "wrong-commit", "tamper", "missing-content"} {
		t.Run(mode, func(t *testing.T) {
			commit := strings.Repeat("a", 40)
			content := "# ADO source\n"
			blob := sourceBlobID([]byte(content))
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/refs") {
					return sourceJSON(t, map[string]interface{}{"value": []map[string]string{{"name": "refs/heads/main-other", "objectId": strings.Repeat("b", 40)}, {"name": "refs/heads/main", "objectId": commit}}}), nil
				}
				q := r.URL.Query()
				if !strings.HasSuffix(r.URL.Path, "/items") || q.Get("path") != "/docs/plan.md" || q.Get("versionDescriptor.version") != commit || q.Get("versionDescriptor.versionType") != "commit" || q.Get("resolveLfs") != "false" || q.Get("recursionLevel") != "none" {
					t.Fatalf("unbounded/unpinned request %s", r.URL)
				}
				item := map[string]interface{}{"commitId": commit, "objectId": blob, "path": "/docs/plan.md", "gitObjectType": "blob", "content": content}
				switch mode {
				case "symlink":
					item["isSymLink"] = true
				case "folder":
					item["isFolder"] = true
				case "submodule":
					item["gitObjectType"] = "commit"
				case "wrong-path":
					item["path"] = "/other.md"
				case "wrong-commit":
					item["commitId"] = strings.Repeat("b", 40)
				case "tamper":
					item["content"] = "same ref different bytes"
				case "missing-content":
					delete(item, "content")
				}
				return sourceJSON(t, item), nil
			})}
			p := NewADOProvider("org", "project", "", func(p *ADOProvider) { p.Client = client })
			repo := RepositoryRef{Provider: ProviderADO, Owner: "org", Project: "project", Name: "repo"}
			head, err := p.ReadSourceBranch(context.Background(), repo, "main")
			if err != nil || head != commit {
				t.Fatalf("exact ref %q %v", head, err)
			}
			file, err := p.ReadRepositorySource(context.Background(), repo, "docs/plan.md", head)
			if mode == "regular" {
				if err != nil || string(file.Content) != content || file.BlobID != blob {
					t.Fatalf("file %+v %v", file, err)
				}
			} else if !errors.Is(err, ErrRepositorySource) {
				t.Fatalf("unsafe file accepted %+v %v", file, err)
			}
		})
	}
}
