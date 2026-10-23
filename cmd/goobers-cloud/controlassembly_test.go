package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestControlAssemblyManifestCoversRealRequiredFields(t *testing.T) {
	fields := reflect.TypeFor[controlPlanes]()
	seen := make(map[string]bool)
	for _, seam := range controlAssembly {
		if seen[seam.field] || seam.issue == "" || seam.present == nil {
			t.Fatalf("invalid manifest entry: %+v", seam)
		}
		seen[seam.field] = true
		if _, ok := fields.FieldByName(seam.field); !ok {
			t.Fatalf("manifest seam %s has no real assembly field", seam.field)
		}
		// Validate the presence test against the actual named field, not a
		// separate hand-maintained list or a constructor's readiness boolean.
		complete := populatedControlPlanes()
		if !complete.complete() || !seam.present(complete) {
			t.Fatal("populated test assembly is incomplete")
		}
		field := reflect.ValueOf(&complete).Elem().FieldByName(seam.field)
		field.Set(reflect.Zero(field.Type()))
		if seam.present(complete) || complete.complete() || len(complete.missing()) != 1 {
			t.Fatalf("%s presence test diverged from its assembly field", seam.field)
		}
	}
	for i := range fields.NumField() {
		field := fields.Field(i).Name
		if field != "RunnerHosts" && !seen[field] {
			t.Fatalf("required assembly field %s is absent from manifest", field)
		}
	}
}

func TestControlCheckAssemblyIsOfflineAndListsPendingSeams(t *testing.T) {
	// Invalid credentials, paths and endpoints must never be inspected by this
	// build-capability probe. Neither the assembly nor a lease is instantiated.
	t.Setenv("GOOBERS_DATABASE_URL", "not a database URL")
	t.Setenv("GOOBERS_DATABASE_DIRECT_URL", "not a database URL")
	t.Setenv("GOOBERS_POD_KEY_FILE", "/does-not-exist/keyring.json")
	prior := assembleControlPlanes
	assembleControlPlanes = func(context.Context, controlDatabases, controlAuth, func() *lease.Handle) (controlPlanes, error) {
		t.Fatal("offline inspection invoked the real dependency constructors")
		return controlPlanes{}, nil
	}
	t.Cleanup(func() { assembleControlPlanes = prior })
	code, stdout, stderr := runForTest("control", "--check-assembly")
	wantCode := 0
	for _, seam := range controlAssembly {
		state := "implemented"
		if seam.construct == nil {
			state, wantCode = "pending", 1
		}
		if !strings.Contains(stdout, seam.name()+"\t"+state+"\thttps://github.com/Agent-Clubhouse/Goobers/issues/"+strings.TrimPrefix(seam.issue, "#")) {
			t.Fatalf("manifest omits %s: %s", seam.field, stdout)
		}
	}
	if code != wantCode || stderr != "" {
		t.Fatalf("offline check code=%d stderr=%s", code, stderr)
	}
}

func TestControlManifestAndRuntimeUseSameConstructors(t *testing.T) {
	prior := controlAssembly
	controlAssembly = append([]controlAssemblySeam(nil), prior...)
	t.Cleanup(func() { controlAssembly = prior })
	calls := make(map[string]int)
	for i := range controlAssembly {
		fieldName := controlAssembly[i].field
		controlAssembly[i].construct = func(_ context.Context, _ databaseServices, _ controlAuth, _ func() *lease.Handle, p *controlPlanes) error {
			calls[fieldName]++
			value := reflect.ValueOf(populatedControlPlanes()).FieldByName(fieldName)
			reflect.ValueOf(p).Elem().FieldByName(fieldName).Set(value)
			return nil
		}
	}
	code, stdout, stderr := runForTest("control", "--check-assembly")
	if code != 0 || stderr != "" || strings.Contains(stdout, "pending") || len(calls) != 0 {
		t.Fatalf("compiled manifest code=%d stdout=%s stderr=%s calls=%v", code, stdout, stderr, calls)
	}
	assembled, err := assembleControlPlanes(t.Context(), controlDatabases{}, controlAuth{}, func() *lease.Handle { return nil })
	if err != nil || !assembled.complete() {
		t.Fatalf("real composition diverged from manifest: %v", err)
	}
	for _, seam := range controlAssembly {
		if calls[seam.field] != 1 {
			t.Fatalf("registered %s constructor called %d times", seam.field, calls[seam.field])
		}
	}
	controlAssembly[0].construct = nil
	code, stdout, _ = runForTest("control", "--check-assembly")
	if code != 1 || !strings.Contains(stdout, controlAssembly[0].name()+"\tpending") {
		t.Fatalf("removing one constructor did not fail closed: code=%d %s", code, stdout)
	}
	controlAssembly[0].construct = func(context.Context, databaseServices, controlAuth, func() *lease.Handle, *controlPlanes) error {
		return nil
	}
	if _, err := assembleControlPlanes(t.Context(), controlDatabases{}, controlAuth{}, func() *lease.Handle { return nil }); err == nil {
		t.Fatal("runtime accepted a registered no-op constructor")
	}
}

// These non-nil interfaces are only composition markers. No method is invoked.
func populatedControlPlanes() controlPlanes {
	return controlPlanes{
		EngineChildAdmissions: httpapi.WithAuthenticator(httpapi.DenyAllAuthenticator{}),
		Configuration:         struct{ control.Configuration }{},
		Reader:                struct{ readservice.Reader }{}, Claims: struct{ httpapi.ClaimService }{},
		Triggers: struct{ httpapi.TriggerService }{}, Journal: struct{ httpapi.JournalService }{},
		RunJournal: struct{ httpapi.RunJournalService }{},
		Resolve:    func(context.Context, triggerqueue.Record) (engine.RunInput, error) { return engine.RunInput{}, nil },
		Epoch: func(context.Context, *lease.Handle) (control.EpochServices, error) {
			return control.EpochServices{}, nil
		},
	}
}
