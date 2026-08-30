package apicontract

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestConfigAuthoringRoutesAreStableAndSeparate(t *testing.T) {
	routes := V1ConfigAuthoringRoutes()
	want := []Route{
		{ID: RouteConfigSources, Method: http.MethodGet, Path: ConfigSourcesPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBounded, Budget: BoundedBudget},
		{ID: RouteConfigSourceDocuments, Method: http.MethodGet, Path: ConfigSourceDocumentsPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBounded, Budget: BoundedBudget},
		{ID: RouteConfigSourceDocument, Method: http.MethodGet, Path: ConfigSourceDocumentPath, ActionClass: ActionReadOnlyNavigation, Cost: CostBounded, Budget: BoundedBudget},
		{ID: RouteConfigSourcePreview, Method: http.MethodPost, Path: ConfigSourcePreviewPath, ActionClass: ActionConfigTime, Cost: CostMutation, Budget: MutationBudget},
		{ID: RouteConfigSourceChanges, Method: http.MethodPut, Path: ConfigSourceChangesPath, ActionClass: ActionConfigTime, Cost: CostMutation, Budget: MutationBudget},
	}
	if err := ValidateRoutes(want, routes); err != nil {
		t.Fatalf("configuration authoring routes: %v", err)
	}
	for _, route := range routes {
		if _, active := V1Route(route.ID); active {
			t.Fatalf("authoring route %q is active before its backing service is implemented", route.ID)
		}
	}
}

func TestConfigAuthoringRoutesReturnCopies(t *testing.T) {
	routes := V1ConfigAuthoringRoutes()
	routes[0].Path = "/changed"
	if got := V1ConfigAuthoringRoutes()[0].Path; got != ConfigSourcesPath {
		t.Fatalf("mutating returned routes changed contract path to %q", got)
	}
}

func TestConfigAuthoringContractRepresentsSourceStrategies(t *testing.T) {
	fixtures := newWireFixtures()
	if len(fixtures.ConfigSources.Items) != 3 {
		t.Fatalf("source fixtures = %d, want local, git, and provider", len(fixtures.ConfigSources.Items))
	}

	got := map[ConfigSourceKind]ConfigSourceCapabilities{}
	for _, source := range fixtures.ConfigSources.Items {
		got[source.Kind] = source.Capabilities
	}
	if !got[ConfigSourceLocal].DirectWrite || got[ConfigSourceLocal].ReviewWrite {
		t.Fatalf("local capabilities = %+v", got[ConfigSourceLocal])
	}
	if !got[ConfigSourceGit].ReviewWrite || got[ConfigSourceGit].DirectWrite {
		t.Fatalf("git capabilities = %+v", got[ConfigSourceGit])
	}
	if !got[ConfigSourceProvider].Read || got[ConfigSourceProvider].DirectWrite || got[ConfigSourceProvider].ReviewWrite {
		t.Fatalf("provider capabilities = %+v", got[ConfigSourceProvider])
	}
}

func TestConfigChangeSetSupportsAtomicMultiDocumentPreview(t *testing.T) {
	fixtures := newWireFixtures()
	request := fixtures.ConfigPreviewRequest
	if request.ChangeSet.BaseRevision == "" {
		t.Fatal("preview request has no base revision")
	}
	if len(request.ChangeSet.Changes) < 2 {
		t.Fatalf("preview changes = %d, want a multi-document candidate", len(request.ChangeSet.Changes))
	}
	for _, change := range request.ChangeSet.Changes {
		if strings.HasPrefix(change.Path, "/") || strings.Contains(change.Path, `:\`) {
			t.Fatalf("change path %q exposes an absolute host path", change.Path)
		}
	}
}

func TestConfigDocumentChangeValidatesDiscriminatedOperations(t *testing.T) {
	empty := ""
	tests := []struct {
		name    string
		change  ConfigDocumentChange
		wantErr string
	}{
		{
			name: "empty upsert content is explicit",
			change: ConfigDocumentChange{
				Path:      "gaggles/core/gaggle.yaml",
				Operation: ConfigChangeUpsert,
				Content:   &empty,
			},
		},
		{
			name: "upsert content is required",
			change: ConfigDocumentChange{
				Path:      "gaggles/core/gaggle.yaml",
				Operation: ConfigChangeUpsert,
			},
			wantErr: "requires content",
		},
		{
			name: "delete requires etag",
			change: ConfigDocumentChange{
				Path:      "gaggles/core/gaggle.yaml",
				Operation: ConfigChangeDelete,
			},
			wantErr: "requires baseEtag",
		},
		{
			name: "delete forbids content",
			change: ConfigDocumentChange{
				Path:      "gaggles/core/gaggle.yaml",
				Operation: ConfigChangeDelete,
				BaseETag:  "sha256:old",
				Content:   &empty,
			},
			wantErr: "cannot include content",
		},
		{
			name: "delete with etag",
			change: ConfigDocumentChange{
				Path:      "gaggles/core/gaggle.yaml",
				Operation: ConfigChangeDelete,
				BaseETag:  "sha256:old",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.change.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}

	data, err := json.Marshal(ConfigDocumentChange{
		Path:      "gaggles/core/gaggle.yaml",
		Operation: ConfigChangeUpsert,
		Content:   &empty,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"content":""`) {
		t.Fatalf("empty upsert content was omitted: %s", data)
	}
}

func TestConfigAuthoringWireShapesExcludeCredentialsAndResolvedPaths(t *testing.T) {
	types := []any{
		ConfigSourcePage{},
		ConfigDocumentPage{},
		ConfigDocumentRequest{},
		ConfigDocument{},
		ConfigChangePreviewRequest{},
		ConfigChangePreview{},
		ConfigWriteRequest{},
		ConfigWriteOutcome{},
		ConfigAuthoringErrorEnvelope{},
	}
	for _, value := range types {
		assertNoForbiddenJSONFields(t, reflect.TypeOf(value), map[reflect.Type]bool{})
	}
}

func assertNoForbiddenJSONFields(t *testing.T, typ reflect.Type, visiting map[reflect.Type]bool) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || visiting[typ] {
		return
	}
	visiting[typ] = true
	defer delete(visiting, typ)

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		lower := strings.ToLower(name)
		for _, forbidden := range []string{
			"credential",
			"token",
			"rootpath",
			"resolvedpath",
			"checkoutpath",
			"absolutepath",
		} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s.%s exposes forbidden JSON field %q", typ, field.Name, name)
			}
		}
		assertNoForbiddenJSONFields(t, field.Type, visiting)
	}
}
