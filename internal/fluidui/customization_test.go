package fluidui

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func validDoc() Customization {
	return Customization{
		Version: Version,
		Density: DensityCompact,
		Pages:   map[string]PageLayout{"runs": {Hidden: []string{"cost"}, Order: []string{"timeline", "gates"}}},
		RunList: &RunList{Sort: SortFailingFirst, Phases: []string{"review"}},
		SavedViews: []SavedView{
			{Name: "My failing runs", Route: "#/runs?status=failed"},
		},
	}
}

func TestDecodeRejectsWithFieldPath(t *testing.T) {
	cases := map[string]struct {
		body  string
		field string
	}{
		"version":        {`{"version":"v0"}`, "version"},
		"unknown field":  {`{"version":"portal.customization/v1","script":"x"}`, "$"},
		"trailing":       {`{"version":"portal.customization/v1"} {}`, "$"},
		"density":        {`{"version":"portal.customization/v1","density":"huge"}`, "density"},
		"panel slug":     {`{"version":"portal.customization/v1","pages":{"runs":{"hidden":["Bad Panel"]}}}`, "pages.runs.hidden[0]"},
		"duplicate":      {`{"version":"portal.customization/v1","pages":{"runs":{"order":["a","a"]}}}`, "pages.runs.order[1]"},
		"sort":           {`{"version":"portal.customization/v1","runList":{"sort":"random"}}`, "runList.sort"},
		"external route": {`{"version":"portal.customization/v1","savedViews":[{"name":"x","route":"https://evil"}]}`, "savedViews[0].route"},
		"script route":   {`{"version":"portal.customization/v1","savedViews":[{"name":"x","route":"#/a\"><script>"}]}`, "savedViews[0].route"},
		"view name":      {`{"version":"portal.customization/v1","savedViews":[{"name":" ","route":"#/a"}]}`, "savedViews[0].name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(tc.body))
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Decode error = %v, want ValidationError", err)
			}
			if verr.Field != tc.field {
				t.Fatalf("field = %q, want %q (%v)", verr.Field, tc.field, err)
			}
		})
	}
	if _, err := Decode([]byte(strings.Repeat(" ", MaxDocumentBytes+1))); err == nil {
		t.Fatal("oversized document accepted")
	}
}

func TestStoreIsolatesUsers(t *testing.T) {
	store := NewStore(t.TempDir())
	alice, bob := UserKey("iss", "alice"), UserKey("iss", "bob")
	if _, err := store.Put(alice, validDoc()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Get(bob)
	if err != nil {
		t.Fatal(err)
	}
	if state.Customization != nil || state.Revision != 0 {
		t.Fatalf("bob sees %+v", state)
	}
	state, err = store.Get(alice)
	if err != nil || state.Customization == nil || state.Customization.Density != DensityCompact {
		t.Fatalf("alice state = %+v, %v", state, err)
	}
	if UserKey("iss", "") != LocalUser {
		t.Fatal("anonymous user is not the local user")
	}
}

func TestStoreResetAndUndo(t *testing.T) {
	store := NewStore(t.TempDir())
	user := UserKey("iss", "alice")
	if _, err := store.Undo(user); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("undo on empty = %v", err)
	}
	first := validDoc()
	second := validDoc()
	second.Density = DensityComfortable
	mustPut(t, store, user, first)
	mustPut(t, store, user, second)

	state, err := store.Reset(user)
	if err != nil || state.Customization != nil || state.UndoDepth != 2 {
		t.Fatalf("reset = %+v, %v", state, err)
	}
	state, err = store.Undo(user)
	if err != nil || state.Customization == nil || state.Customization.Density != DensityComfortable {
		t.Fatalf("undo after reset = %+v, %v", state, err)
	}
	state, err = store.Undo(user)
	if err != nil || state.Customization.Density != DensityCompact || state.UndoDepth != 0 {
		t.Fatalf("second undo = %+v, %v", state, err)
	}
	if state.Revision != 5 {
		t.Fatalf("revision = %d, want 5", state.Revision)
	}
}

func TestStoreCapsHistory(t *testing.T) {
	store := NewStore(t.TempDir())
	clock := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	user := UserKey("iss", "alice")
	for i := 0; i < MaxHistory+5; i++ {
		mustPut(t, store, user, validDoc())
	}
	state, err := store.Get(user)
	if err != nil || state.UndoDepth != MaxHistory {
		t.Fatalf("state = %+v, %v", state, err)
	}
}

func TestStoreReportsIncompatibleDocument(t *testing.T) {
	store := NewStore(t.TempDir())
	user := UserKey("iss", "alice")
	mustPut(t, store, user, validDoc())
	path := store.path(user)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), Version, "portal.customization/v9", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Get(user)
	if err != nil || !state.Incompatible || state.Customization != nil {
		t.Fatalf("state = %+v, %v", state, err)
	}
}

func TestPutRejectsInvalidDocument(t *testing.T) {
	store := NewStore(t.TempDir())
	doc := validDoc()
	doc.Version = ""
	if _, err := store.Put(UserKey("iss", "alice"), doc); err == nil {
		t.Fatal("invalid document stored")
	}
}

func mustPut(t *testing.T, store *Store, user string, doc Customization) {
	t.Helper()
	if _, err := store.Put(user, doc); err != nil {
		t.Fatal(err)
	}
}
