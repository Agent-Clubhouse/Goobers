package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestADOCompareCommitsPagesAllChanges(t *testing.T) {
	for _, flagged := range []bool{false, true} {
		t.Run(fmt.Sprintf("completion_flag_%t", flagged), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				wantSkip := calls * adoChangePageSize
				calls++
				if q.Get("$skip") != strconv.Itoa(wantSkip) || q.Get("$top") != strconv.Itoa(adoChangePageSize) || q.Get("baseVersion") != "base" || q.Get("targetVersion") != "head" || q.Get("baseVersionType") != "commit" || q.Get("targetVersionType") != "commit" {
					t.Errorf("unexpected query: %s", r.URL.RawQuery)
				}
				if calls > 2 {
					http.Error(w, "unexpected page", 400)
					return
				}
				count := adoChangePageSize
				if calls == 2 && !flagged {
					count = 1
				}
				changes := make([]map[string]interface{}, count)
				for i := range changes {
					changes[i] = map[string]interface{}{"changeType": "edit", "item": map[string]interface{}{"path": fmt.Sprintf("/file-%d", wantSkip+i), "isFolder": calls == 1 && i == 0}}
				}
				body := map[string]interface{}{"commonCommit": "ancestor", "changes": changes}
				if flagged {
					body["allChangesIncluded"] = calls == 2
				}
				writeJSON(t, w, body)
			}))
			defer server.Close()
			p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			got, err := p.CompareCommits(context.Background(), adoLandingRepo(), "base", "head")
			wantFiles := adoChangePageSize
			if flagged {
				wantFiles = 2*adoChangePageSize - 1
			}
			if err != nil || calls != 2 || len(got.Files) != wantFiles || got.MergeBaseSHA != "ancestor" {
				t.Fatalf("files=%d base=%s calls=%d err=%v", len(got.Files), got.MergeBaseSHA, calls, err)
			}
			if got.Files[len(got.Files)-1].Path != fmt.Sprintf("file-%d", wantFiles) {
				t.Fatal("last page omitted")
			}
		})
	}
}

func TestADOCompareCommitsIncompleteShortPage(t *testing.T) {
	for _, ending := range []string{"complete", "empty", "failure", "changed base"} {
		t.Run(ending, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					writeJSON(t, w, map[string]interface{}{"commonCommit": "ancestor", "allChangesIncluded": false, "changes": []map[string]interface{}{{"changeType": "add", "item": map[string]interface{}{"path": "/first"}}}})
					return
				}
				if calls > 2 {
					http.Error(w, "unexpected page", 400)
					return
				}
				if r.URL.Query().Get("$skip") != "1" {
					t.Errorf("offset=%s", r.URL.Query().Get("$skip"))
				}
				switch ending {
				case "failure":
					http.Error(w, "second page unavailable", http.StatusBadRequest)
				case "empty":
					writeJSON(t, w, map[string]interface{}{"commonCommit": "ancestor", "allChangesIncluded": false, "changes": []interface{}{}})
				default:
					base := "ancestor"
					if ending == "changed base" {
						base = "different"
					}
					writeJSON(t, w, map[string]interface{}{"commonCommit": base, "allChangesIncluded": true, "changes": []map[string]interface{}{{"changeType": "delete", "item": map[string]interface{}{"path": "/last"}}}})
				}
			}))
			defer server.Close()
			p := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			got, err := p.CompareCommits(context.Background(), adoLandingRepo(), "base", "head")
			if ending == "complete" {
				if err != nil || len(got.Files) != 2 || got.Files[1].Status != "removed" {
					t.Fatalf("got=%+v err=%v", got, err)
				}
			} else if err == nil || len(got.Files) != 0 || got.MergeBaseSHA != "" {
				t.Fatalf("partial success: %+v %v", got, err)
			}
			if calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
