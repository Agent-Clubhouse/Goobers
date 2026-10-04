package workbenchprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

type nativeEditFixture struct {
	kind                      apiv1.Provider
	version, reads, patches   int
	title, description, state string
	labels, assignees         []string
	lost, intervening         bool
	t                         *testing.T
}

func newEditFixture(t *testing.T, kind apiv1.Provider) (*BacklogWriter, *nativeEditFixture) {
	t.Helper()
	f := &nativeEditFixture{kind: kind, version: 1, title: "Old", description: "old body", state: "open", labels: []string{"bug", "goobers:needs-human", "goobers:claim-run:legacy"}, assignees: []string{"one"}, t: t}
	if kind == apiv1.ProviderADO {
		f.state = "Active"
	}
	client := &http.Client{Transport: roundTripFunc(f.roundTrip)}
	var native NativeWriter
	if kind == apiv1.ProviderGitHub {
		native = providers.NewGitHubProvider("", providers.WithHTTPClient(client), providers.WithMaxTransientRetries(3))
	} else {
		native = providers.NewADOProvider("org", "project", "", func(p *providers.ADOProvider) { p.Client = client })
	}
	scope, bound := source(kind)
	bound.Spec.Writes = &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"title", "description", "state", "labels", "assignees"}}
	writer, err := NewBacklogWriter(scope, bound, native)
	if err != nil {
		t.Fatal(err)
	}
	return writer, f
}

func (f *nativeEditFixture) item() string {
	var payload map[string]interface{}
	if f.kind == apiv1.ProviderGitHub {
		labels := []map[string]string{}
		for _, label := range f.labels {
			labels = append(labels, map[string]string{"name": label})
		}
		users := []map[string]string{}
		for _, name := range f.assignees {
			users = append(users, map[string]string{"login": name})
		}
		payload = map[string]interface{}{"id": 101, "number": 7, "title": f.title, "body": f.description, "state": f.state, "labels": labels, "assignees": users, "html_url": "https://github.com/org/repo/issues/7", "updated_at": fmt.Sprintf("2026-10-01T01:00:%02dZ", f.version)}
	} else {
		fields := map[string]interface{}{"System.TeamProject": "project", "System.Title": f.title, "System.Description": f.description, "System.State": f.state, "System.Tags": strings.Join(f.labels, "; "), "System.WorkItemType": "Epic", "Microsoft.VSTS.Common.AcceptanceCriteria": "keep criteria"}
		if len(f.assignees) > 0 {
			fields["System.AssignedTo"] = map[string]string{"displayName": "Display", "uniqueName": f.assignees[0]}
		}
		payload = map[string]interface{}{"id": 101, "rev": f.version, "url": "https://dev.azure.com/org/_apis/wit/workItems/101", "fields": fields, "relations": []interface{}{}}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(raw)
}

func (f *nativeEditFixture) roundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/states") {
		return response(`{"value":[{"name":"Active","category":"InProgress"},{"name":"Closed","category":"Completed"}]}`), nil
	}
	switch r.Method {
	case http.MethodGet:
		f.reads++
		if f.intervening && f.reads == 2 {
			f.version++
			f.title = "someone else's edit"
		}
		return response(f.item()), nil
	case http.MethodPatch:
		f.patches++
		if f.kind == apiv1.ProviderGitHub {
			f.applyGitHub(r.Body)
		} else {
			f.applyADO(r.Body)
		}
		f.version++
		if f.lost {
			// The provider committed but its success was lost. A 503 would be
			// automatically replayed by the legacy PATCH retry path.
			out := response(`{"message":"unavailable"}`)
			out.StatusCode = 503
			return out, nil
		}
		return response(f.item()), nil
	default:
		f.t.Fatalf("unexpected compound mutation %s %s", r.Method, r.URL)
		return nil, errors.New("unexpected")
	}
}

func (f *nativeEditFixture) applyGitHub(body io.Reader) {
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(body).Decode(&patch); err != nil {
		f.t.Fatal(err)
	}
	if len(patch) != 1 {
		f.t.Fatalf("not one field: %v", patch)
	}
	for field, value := range patch {
		switch field {
		case "title":
			decodeEdit(f.t, value, &f.title)
		case "body":
			decodeEdit(f.t, value, &f.description)
		case "state":
			decodeEdit(f.t, value, &f.state)
		case "labels":
			decodeEdit(f.t, value, &f.labels)
		case "assignees":
			decodeEdit(f.t, value, &f.assignees)
		default:
			f.t.Fatalf("unexpected field %s", field)
		}
	}
}
func decodeEdit(t *testing.T, raw []byte, value interface{}) {
	t.Helper()
	if err := json.Unmarshal(raw, value); err != nil {
		t.Fatal(err)
	}
}

func (f *nativeEditFixture) applyADO(body io.Reader) {
	var patch []struct {
		Op, Path string
		Value    json.RawMessage
	}
	if err := json.NewDecoder(body).Decode(&patch); err != nil {
		f.t.Fatal(err)
	}
	if len(patch) != 2 || patch[0].Op != "test" || patch[0].Path != "/rev" || string(patch[0].Value) != fmt.Sprint(f.version) || patch[1].Op != "add" {
		f.t.Fatalf("non-atomic or compound patch: %+v", patch)
	}
	var value string
	decodeEdit(f.t, patch[1].Value, &value)
	switch patch[1].Path {
	case "/fields/System.Title":
		f.title = value
	case "/fields/System.Description":
		f.description = value
	case "/fields/System.State":
		f.state = value
	case "/fields/System.Tags":
		f.labels = strings.Split(value, "; ")
	case "/fields/System.AssignedTo":
		f.assignees = nil
		if value != "" {
			f.assignees = []string{value}
		}
	default:
		f.t.Fatalf("unexpected field side effect %s", patch[1].Path)
	}
}

func editRequest(kind apiv1.Provider, field apiv1.WorkbenchField) workbench.BacklogPatchRequest {
	id, revision := "7", "2026-10-01T01:00:01Z"
	if kind == apiv1.ProviderADO {
		id, revision = "101", "1"
	}
	request := workbench.BacklogPatchRequest{ID: id, SourceID: "101", ExpectedRevision: revision, Field: field}
	value := "new native content"
	switch field {
	case "title", "description":
		request.Value = &value
	case "state":
		value = "closed"
		if kind == apiv1.ProviderADO {
			value = "Closed"
		}
		request.Value = &value
	case "labels":
		request.Values = []string{"feature", "goobers:needs-human", "goobers:claim-run:legacy"}
	case "assignees":
		request.Values = []string{"second"}
		if kind == apiv1.ProviderGitHub {
			request.Values = append(request.Values, "third")
		}
	}
	return request
}

func TestNativeFieldEditsUseOneMutationAndVerify(t *testing.T) {
	for _, kind := range []apiv1.Provider{apiv1.ProviderGitHub, apiv1.ProviderADO} {
		for _, field := range []apiv1.WorkbenchField{"title", "description", "state", "labels", "assignees"} {
			t.Run(string(kind)+"/"+string(field), func(t *testing.T) {
				writer, f := newEditFixture(t, kind)
				request := editRequest(kind, field)
				digest, err := writer.OperationDigest(request)
				if err != nil || f.reads != 0 || f.patches != 0 {
					t.Fatalf("digest must be pure: %s %v", digest, err)
				}
				receipt, err := writer.Patch(context.Background(), request)
				if err != nil || receipt.Outcome != "confirmed" || !receipt.ProviderAcknowledged || !receipt.ObservedMatches || receipt.OperationDigest != digest || f.patches != 1 {
					t.Fatalf("receipt=%+v err=%v patches=%d", receipt, err, f.patches)
				}
				if kind == apiv1.ProviderADO && receipt.Observed.AcceptanceCriteria != "keep criteria" {
					t.Fatal("description edit lost criteria")
				}
				if field != "labels" && !slices.Contains(receipt.Observed.Labels, "goobers:claim-run:legacy") {
					t.Fatal("unrelated labels changed")
				}
			})
		}
	}
}

func TestUnknownNativeEditDoesNotReplayOrClaimAuthorship(t *testing.T) {
	for _, kind := range []apiv1.Provider{apiv1.ProviderGitHub, apiv1.ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			writer, f := newEditFixture(t, kind)
			f.lost = true
			receipt, err := writer.Patch(context.Background(), editRequest(kind, "title"))
			if err == nil || receipt.Outcome != "unknown" || receipt.ProviderAcknowledged || !receipt.ObservedMatches || f.patches != 1 {
				t.Fatalf("lost ack replay/false authorship %+v %v count=%d", receipt, err, f.patches)
			}
		})
	}
}

func TestNativeEditPreflightRefusals(t *testing.T) {
	for _, kind := range []apiv1.Provider{apiv1.ProviderGitHub, apiv1.ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			writer, f := newEditFixture(t, kind)
			f.intervening = true
			receipt, err := writer.Patch(context.Background(), editRequest(kind, "title"))
			var conflict *providers.RevisionConflictError
			if !errors.As(err, &conflict) || receipt.Outcome != "not-applied" || f.patches != 0 {
				t.Fatalf("intervening edit overwritten %+v %v", receipt, err)
			}
			writer, f = newEditFixture(t, kind)
			request := editRequest(kind, "labels")
			request.Values = []string{"feature"}
			receipt, err = writer.Patch(context.Background(), request)
			if !errors.Is(err, providers.ErrNativeEdit) || receipt.Outcome != "not-applied" || f.patches != 0 {
				t.Fatalf("service controls silently cleared %+v %v", receipt, err)
			}
		})
	}
}

func TestNativeEditValidationPrecedesProviderReads(t *testing.T) {
	writer, f := newEditFixture(t, apiv1.ProviderGitHub)
	cases := []workbench.BacklogPatchRequest{
		{ID: "7", SourceID: "101", Field: "title"},
		{ID: "7", SourceID: "101", ExpectedRevision: "r", Field: "labels", Values: []string{"a", "A"}},
		{ID: "7", SourceID: "101", ExpectedRevision: "r", Field: "assignees", Values: make([]string, 11)},
		{ID: "7", SourceID: "101", ExpectedRevision: "r", Field: "state", Value: new("done")},
		{ID: "7", ExpectedRevision: "r", Field: "title", Value: new("missing identity")},
	}
	for _, request := range cases {
		if _, err := writer.Patch(context.Background(), request); err == nil {
			t.Fatalf("invalid edit accepted %+v", request)
		}
	}
	writer.fields = nil
	if _, err := writer.Patch(context.Background(), editRequest(apiv1.ProviderGitHub, "title")); !errors.Is(err, ErrUnsupportedEdit) {
		t.Fatalf("read-only source edited %v", err)
	}
	if f.reads != 0 || f.patches != 0 {
		t.Fatalf("invalid payload made calls %d/%d", f.reads, f.patches)
	}
}

func TestNativeEditReadbackMismatchIsNotConfirmed(t *testing.T) {
	writer, f := newEditFixture(t, apiv1.ProviderGitHub)
	base := writer.client.(*providers.GitHubProvider)
	base.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && f.patches > 0 {
			f.title = "intervening edit after acknowledgment"
		}
		return f.roundTrip(r)
	})}
	receipt, err := writer.Patch(context.Background(), editRequest(apiv1.ProviderGitHub, "title"))
	if err == nil || receipt.Outcome != "unknown" || !receipt.ProviderAcknowledged || receipt.ObservedMatches || f.patches != 1 {
		t.Fatalf("acknowledgment falsely implied convergence: %+v %v", receipt, err)
	}
}

func TestPureEditMetadataMatchesWriterWithoutProviderReads(t *testing.T) {
	for _, kind := range []apiv1.Provider{apiv1.ProviderGitHub, apiv1.ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			writer, fixture := newEditFixture(t, kind)
			scope, bound := source(kind)
			bound.Spec.Writes = &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"title", "description", "state", "labels", "assignees"}}
			value := "New title"
			request := workbench.BacklogPatchRequest{ID: "7", SourceID: "101", ExpectedRevision: "2026-10-01T01:00:01Z", Field: "title", Value: &value}
			if kind == apiv1.ProviderADO {
				request.ID = "101"
				request.ExpectedRevision = "1"
			}
			expected, err := writer.OperationDigest(request)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := BacklogOperationDigest(scope, bound, request)
			if err != nil || actual != expected {
				t.Fatalf("pure digest differs: %s %s %v", expected, actual, err)
			}
			bound.Spec.Objectives = &apiv1.WorkbenchObjectiveSelector{IDs: []string{"999"}, Types: []string{"Feature"}}
			classified, err := BacklogOperationDigest(scope, bound, request)
			if err != nil || classified != expected {
				t.Fatal("classification edit invalidated replay", err)
			}
			if !reflect.DeepEqual(writer.Capabilities(), BacklogCapabilities(bound)) {
				t.Fatal("pure capabilities differ")
			}
			bound.Spec.Writes = nil
			if _, err := BacklogOperationDigest(scope, bound, request); !errors.Is(err, ErrUnsupportedEdit) {
				t.Fatal("missing allowlist accepted", err)
			}
			if fixture.reads != 0 || fixture.patches != 0 {
				t.Fatal("pure metadata contacted provider")
			}
		})
	}
}
