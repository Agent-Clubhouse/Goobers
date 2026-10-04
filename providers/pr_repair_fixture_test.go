package providers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

const repairOldSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const repairNewSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const repairOldTree = "cccccccccccccccccccccccccccccccccccccccc"
const repairNewTree = "dddddddddddddddddddddddddddddddddddddddd"
const repairRepoID = "11111111-2222-3333-4444-555555555555"

type repairFixture struct {
	t             *testing.T
	kind          ProviderKind
	mode          string
	head          string
	attempts      int
	intent        PullRequestRepair
	before, after map[string]string
}

func newRepairFixture(t *testing.T, kind ProviderKind) (*repairFixture, interface {
	PRRepairReader
	PRRepairWriter
}) {
	t.Helper()
	f := &repairFixture{t: t, kind: kind, head: repairOldSHA, before: map[string]string{"edit.txt": "before", "delete.txt": "remove"}, after: map[string]string{"edit.txt": "after", "added.txt": "new"}}
	client := &http.Client{Transport: attentionTransport(f.roundTrip)}
	if kind == ProviderGitHub {
		return f, NewGitHubProvider("human", WithHTTPClient(client))
	}
	return f, NewADOProvider("acme", "project", "human", func(p *ADOProvider) { p.Client = client })
}
func (f *repairFixture) repo() RepositoryRef {
	return RepositoryRef{Provider: f.kind, Owner: "acme", Project: map[ProviderKind]string{ProviderADO: "project"}[f.kind], Name: "app"}
}
func (f *repairFixture) json(r *http.Request, status int, value interface{}) *http.Response {
	f.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	return attentionResponse(r, status, string(data))
}
func (f *repairFixture) pr() map[string]interface{} {
	if f.kind == ProviderGitHub {
		repo := map[string]interface{}{"id": 77, "full_name": "acme/app"}
		headRepo := repo
		if f.mode == "foreign-repo" {
			repo = map[string]interface{}{"id": 77, "full_name": "foreign/app"}
			headRepo = repo
		}
		if f.mode == "fork" {
			headRepo = map[string]interface{}{"id": 88, "full_name": "foreign/app"}
		}
		return map[string]interface{}{"id": 99, "number": 42, "title": "Repair", "body": "A PR", "state": "open", "head": map[string]interface{}{"ref": "work", "sha": f.head, "repo": headRepo}, "base": map[string]interface{}{"ref": "main", "sha": repairOldSHA, "repo": repo}}
	}
	result := map[string]interface{}{"pullRequestId": 42, "title": "Repair", "description": "A PR", "status": "active", "sourceRefName": "refs/heads/work", "targetRefName": "refs/heads/main", "lastMergeSourceCommit": map[string]string{"commitId": f.head}, "lastMergeTargetCommit": map[string]string{"commitId": repairOldSHA}, "repository": map[string]interface{}{"id": repairRepoID, "name": "app", "remoteUrl": "https://dev.azure.com/acme/project/_git/app", "project": map[string]string{"name": "project"}}}
	if f.mode == "foreign-repo" {
		result["repository"].(map[string]interface{})["name"] = "foreign"
	}
	if f.mode == "foreign-org" {
		result["repository"].(map[string]interface{})["remoteUrl"] = "https://dev.azure.com/foreign/project/_git/app"
	}
	if f.mode == "foreign-project" {
		result["repository"].(map[string]interface{})["project"] = map[string]string{"name": "foreign"}
	}
	if f.mode == "fork" {
		result["forkSource"] = map[string]string{"name": "other"}
	}
	return result
}
func (f *repairFixture) files(sha string) map[string]string {
	if sha == repairNewSHA || sha == repairNewTree {
		return f.after
	}
	return f.before
}
func (f *repairFixture) roundTrip(r *http.Request) (*http.Response, error) {
	f.t.Helper()
	if r.Method != http.MethodGet {
		return f.mutation(r)
	}
	path := r.URL.Path
	if strings.Contains(path, "/pulls/") || strings.Contains(path, "/pullrequests/") {
		return f.json(r, 200, f.pr()), nil
	}
	if strings.HasSuffix(path, "/changes") {
		return f.changed(r), nil
	}
	parts := strings.Split(path, "/")
	id := parts[len(parts)-1]
	if strings.Contains(path, "/commits/") {
		return f.commit(r, id), nil
	}
	if strings.Contains(path, "/trees/") {
		return f.tree(r, id), nil
	}
	if strings.Contains(path, "/blobs/") {
		for _, files := range []map[string]string{f.before, f.after} {
			for _, text := range files {
				if sourceBlobID([]byte(text)) == id {
					return f.json(r, 200, map[string]interface{}{"sha": id, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(text)), "size": len(text)}), nil
				}
			}
		}
		f.t.Fatal("unexpected blob", id)
	}
	if strings.HasSuffix(path, "/items") {
		name := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
		sha := r.URL.Query().Get("versionDescriptor.version")
		text, found := f.files(sha)[name]
		if !found {
			f.t.Fatal("missing file fetched instead of tree absence", name)
		}
		return f.json(r, 200, map[string]interface{}{"commitId": sha, "path": "/" + name, "objectId": sourceBlobID([]byte(text)), "gitObjectType": "blob", "content": text}), nil
	}
	return nil, fmt.Errorf("unexpected repair read %s", r.URL)
}
func (f *repairFixture) tree(r *http.Request, id string) *http.Response {
	if f.mode == "denied-tree" {
		return f.json(r, 403, map[string]string{"message": "denied"})
	}
	entries := []map[string]interface{}{}
	files := f.files(id)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		mode := "100644"
		if f.mode == "symlink" {
			mode = "120000"
		}
		if f.mode == "executable" {
			mode = "100755"
		}
		text := files[name]
		if f.kind == ProviderGitHub {
			entries = append(entries, map[string]interface{}{"path": name, "mode": mode, "type": "blob", "sha": sourceBlobID([]byte(text)), "size": len(text)})
		} else {
			entries = append(entries, map[string]interface{}{"relativePath": name, "mode": mode, "gitObjectType": "blob", "objectId": sourceBlobID([]byte(text)), "size": len(text)})
		}
	}
	if f.kind == ProviderGitHub {
		return f.json(r, 200, map[string]interface{}{"sha": id, "tree": entries, "truncated": f.mode == "truncated"})
	}
	response := f.json(r, 200, map[string]interface{}{"objectId": id, "treeEntries": entries})
	if f.mode == "truncated" {
		response.Header.Set("x-ms-continuationtoken", "more")
	}
	return response
}
func (f *repairFixture) commit(r *http.Request, id string) *http.Response {
	tree := repairOldTree
	if id == repairNewSHA {
		tree = repairNewTree
	}
	message := f.intent.Message
	if f.mode == "wrong-message" {
		message = "foreign commit"
	}
	if f.kind == ProviderADO {
		return f.json(r, 200, map[string]interface{}{"commitId": id, "treeId": tree, "comment": message, "parents": []string{repairOldSHA}})
	}
	value := map[string]interface{}{"sha": id, "tree": map[string]string{"sha": tree}}
	if !strings.Contains(r.URL.Path, "/git/commits/") {
		value["parents"] = []map[string]string{{"sha": repairOldSHA}}
		value["commit"] = map[string]string{"message": message}
		paths := []map[string]string{}
		for _, change := range f.intent.Changes {
			paths = append(paths, map[string]string{"filename": change.Path, "status": "modified"})
		}
		if f.mode == "extra-change" {
			paths = append(paths, map[string]string{"filename": "foreign.txt", "status": "added"})
		}
		value["files"] = paths
	}
	return f.json(r, 200, value)
}
func (f *repairFixture) changed(r *http.Request) *http.Response {
	changes := []map[string]interface{}{}
	counts := map[string]int{}
	for _, change := range f.intent.Changes {
		kind := "edit"
		if change.PreviousBlob == "" {
			kind = "add"
		}
		if change.Content == nil {
			kind = "delete"
		}
		counts[strings.ToUpper(kind[:1])+kind[1:]]++
		changes = append(changes, map[string]interface{}{"changeType": kind, "item": map[string]string{"path": "/" + change.Path, "gitObjectType": "blob"}})
	}
	if f.mode == "extra-change" {
		counts["Add"]++
		changes = append(changes, map[string]interface{}{"changeType": "add", "item": map[string]string{"path": "/foreign.txt", "gitObjectType": "blob"}})
	}
	return f.json(r, 200, map[string]interface{}{"changeCounts": counts, "changes": changes})
}
func (f *repairFixture) mutation(r *http.Request) (*http.Response, error) {
	f.attempts++
	if f.kind == ProviderGitHub {
		if r.URL.Path != "/graphql" {
			f.t.Fatal("not one native commit mutation", r.URL)
		}
		var body struct {
			Query     string
			Variables struct {
				Input struct {
					ExpectedHeadOID string
					Branch          struct{ RepositoryNameWithOwner, BranchName string }
					Message         struct{ Headline, Body string }
					FileChanges     struct {
						Additions []struct{ Path, Contents string }
						Deletions []struct{ Path string }
					}
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Fatal(err)
		}
		in := body.Variables.Input
		if !strings.HasPrefix(body.Query, "mutation") || in.ExpectedHeadOID != repairOldSHA || in.Branch.BranchName != "work" || in.Branch.RepositoryNameWithOwner != "acme/app" || len(in.FileChanges.Additions) != 2 || len(in.FileChanges.Deletions) != 1 || in.Message.Headline+"\n\n"+in.Message.Body != f.intent.Message {
			f.t.Fatal("wrong mutation contract", body)
		}
	} else {
		if !strings.HasSuffix(r.URL.Path, "/pushes") {
			f.t.Fatal("not one native push", r.URL)
		}
		var body adoPushRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Fatal(err)
		}
		if len(body.RefUpdates) != 1 || body.RefUpdates[0].OldObjectID != repairOldSHA || body.RefUpdates[0].Name != "refs/heads/work" || len(body.Commits) != 1 || body.Commits[0].Comment != f.intent.Message || len(body.Commits[0].Changes) != 3 {
			f.t.Fatal("wrong push contract", body)
		}
	}
	if f.mode == "race" {
		f.head = strings.Repeat("e", 40)
		return f.json(r, 409, map[string]string{"message": "head changed"}), nil
	}
	f.head = repairNewSHA
	if f.mode == "lost" {
		return f.json(r, 503, map[string]string{"message": "response lost"}), nil
	}
	if f.kind == ProviderGitHub {
		return f.json(r, 200, map[string]interface{}{"data": map[string]interface{}{"createCommitOnBranch": map[string]interface{}{"commit": map[string]interface{}{"oid": repairNewSHA, "message": f.intent.Message, "parents": map[string]interface{}{"nodes": []map[string]string{{"oid": repairOldSHA}}}}, "ref": map[string]interface{}{"name": "work", "prefix": "refs/heads/", "target": map[string]string{"oid": repairNewSHA}}}}}), nil
	}
	return f.json(r, 201, map[string]interface{}{"commits": []map[string]interface{}{{"commitId": repairNewSHA, "treeId": repairNewTree, "comment": f.intent.Message, "parents": []string{repairOldSHA}}}, "refUpdates": []map[string]string{{"name": "refs/heads/work", "oldObjectId": repairOldSHA, "newObjectId": repairNewSHA}}}), nil
}
