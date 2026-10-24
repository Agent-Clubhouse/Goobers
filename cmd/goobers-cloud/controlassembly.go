package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/cloud/childengine"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/enginechildplane"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

type controlPlaneConstructor func(context.Context, databaseServices, controlAuth, *lease.Manager, func() *lease.Handle, *controlPlanes) error

type controlAssemblySeam struct {
	field, issue string
	label        string
	controller   bool
	present      func(controlPlanes) bool
	construct    controlPlaneConstructor
	// requires names the role option the constructor cannot work without, so the
	// offline check reports what this invocation could actually compose.
	requires func(controlOptions) string
}

func requiresConfigDirectory(o controlOptions) string {
	if strings.TrimSpace(o.configDir) == "" {
		return "--config-dir"
	}
	return requiresObjectStore(o)
}

func requiresObjectStore(o controlOptions) string {
	if strings.TrimSpace(o.objectStoreURL) == "" {
		return "--object-store-url"
	}
	return ""
}

// This is both the compiled manifest and the real assembly registry. Backend
// integration registers its constructor here; a separate readiness boolean or
// hand-maintained list of implemented seams cannot claim success. Nil means
// pending, never a successful no-op constructor.
var controlAssembly = []controlAssemblySeam{
	{field: "Configuration", label: "admission guard", issue: "#7860", controller: true, present: func(p controlPlanes) bool { return p.Configuration != nil }, construct: constructConfiguration, requires: requiresConfigDirectory},
	// After Configuration: freezing a child resolves its pinned generation
	// against the applied configuration.
	{field: "EngineChildAdmissions", label: "engine child admissions", issue: "#7879", controller: true, present: func(p controlPlanes) bool { return p.EngineChildAdmissions != nil }, construct: constructEngineChildAdmissions, requires: requiresObjectStore},
	// After EngineChildAdmissions: grants bind in the same fenced custody.
	{field: "EngineChildAuthority", label: "engine child authority", issue: "#8085", controller: true, present: func(p controlPlanes) bool { return p.EngineChildAuthority != nil }, construct: constructEngineChildAuthority, requires: requiresObjectStore},
	{field: "EngineChildProtocol", label: "engine child protocol", issue: "#8085", controller: true, present: func(p controlPlanes) bool { return p.EngineChildProtocol != nil }, construct: constructEngineChildProtocol, requires: requiresObjectStore},
	// After EngineChildAuthority: every child-workflow call re-resolves the
	// parent through the same authority and checks the grant it bound.
	{field: "ChildWorkflows", label: "child workflows", issue: "#8085", controller: true, present: func(p controlPlanes) bool { return p.ChildWorkflows != nil }, construct: constructChildWorkflows, requires: requiresObjectStore},
	{field: "Reader", issue: "#7811", present: func(p controlPlanes) bool { return p.Reader != nil }},
	{field: "Claims", issue: "#7818", controller: true, present: func(p controlPlanes) bool { return p.Claims != nil }, construct: constructClaims},
	{field: "Triggers", issue: "#7818", present: func(p controlPlanes) bool { return p.Triggers != nil }, construct: constructTriggers},
	{field: "Journal", issue: "#7818", controller: true, present: func(p controlPlanes) bool { return p.Journal != nil }, construct: constructJournal, requires: requiresObjectStore},
	{field: "RunJournal", issue: "#7818", present: func(p controlPlanes) bool { return p.RunJournal != nil }},
	{field: "Resolve", controller: true, issue: "#7818", present: func(p controlPlanes) bool { return p.Resolve != nil }},
	{field: "Epoch", controller: true, issue: "#7818", present: func(p controlPlanes) bool { return p.Epoch != nil }, construct: constructEpoch},
	// After Configuration, Journal and Epoch: the scheduler admission reads the
	// applied generation, journals through the fenced instance log, and feeds the
	// observations ledger the Epoch seam persists.
	{field: "Admission", label: "scheduler admission", controller: true, issue: "#7818", present: func(p controlPlanes) bool { return p.Admission != nil }, construct: constructScheduler, requires: requiresConfigDirectory},
}

func composeControlPlanes(ctx context.Context, db controlDatabases, auth controlAuth, manager *lease.Manager, owner func() *lease.Handle) (controlPlanes, error) {
	var planes controlPlanes
	for _, seam := range controlAssembly {
		if seam.construct == nil {
			continue
		}
		database := db.API
		if seam.controller {
			database = db.Controller
		}
		if err := seam.construct(ctx, database, auth, manager, owner, &planes); err != nil {
			planes.close()
			return controlPlanes{}, fmt.Errorf("compose control %s: %w", seam.field, err)
		}
		if !seam.present(planes) {
			planes.close()
			return controlPlanes{}, fmt.Errorf("control %s constructor did not compose its required seam", seam.field)
		}
	}
	return planes, nil
}

// checkControlAssembly reports each seam as implemented only when its
// constructor exists and the options of this invocation satisfy it; a seam that
// would refuse at compose time for a missing option is pending, with the option
// named in a fourth column.
func checkControlAssembly(out io.Writer, o controlOptions) int {
	code := 0
	for _, seam := range controlAssembly {
		state, needs := "implemented", ""
		if seam.construct == nil {
			state = "pending"
			code = 1
		} else if seam.requires != nil {
			if missing := seam.requires(o); missing != "" {
				state, needs = "pending", "\tneeds "+missing
				code = 1
			}
		}
		writef(out, "%s\t%s\thttps://github.com/Agent-Clubhouse/Goobers/issues/%s%s\n", seam.name(), state, strings.TrimPrefix(seam.issue, "#"), needs)
	}
	return code
}

func (p controlPlanes) missing() []string {
	var missing []string
	for _, seam := range controlAssembly {
		if !seam.present(p) {
			missing = append(missing, seam.field+" ("+seam.issue+")")
		}
	}
	return missing
}

func validateControlAssembly(p controlPlanes) error {
	if missing := p.missing(); len(missing) > 0 {
		return fmt.Errorf("cloud PG plane and recovery assembly incomplete: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (s controlAssemblySeam) name() string {
	if s.label != "" {
		return s.label
	}
	return s.field
}

// constructEngineChildAdmissions serves generated-child custody to engine
// workers from the live controller lease (#7879).
func constructEngineChildAdmissions(_ context.Context, db databaseServices, _ controlAuth, manager *lease.Manager, owner func() *lease.Handle, p *controlPlanes) error {
	applied, ok := p.Configuration.(childengine.AppliedConfiguration)
	if !ok {
		return errors.New("engine child admissions require the applied configuration (#7860)")
	}
	if db.Pools == nil {
		return errors.New("engine child admissions require the controller database")
	}
	admissions := childengine.Fenced{Pool: db.Pools.Transaction, InstanceID: db.InstanceID, Lease: manager, Owner: owner, Blobs: db.Blobs, Objects: db.Objects,
		Applied: applied, Wait: childengine.MaxWait, Actor: "goobers-cloud/control", Now: time.Now, Adoption: &childengine.EpochAdoption{}}
	if err := admissions.Validate(); err != nil {
		return fmt.Errorf("%w (control needs --object-store-url)", err)
	}
	plane, err := enginechildplane.New(admissions)
	if err != nil {
		return err
	}
	p.EngineChildAdmissions = httpapi.WithEngineChildAdmissions(plane)
	p.engineChildren = &admissions
	return nil
}

// constructEngineChildAuthority binds engine parents' child-workflow grants
// for engine workers (#8085 b), from the run pins, the control-side journal and
// the live Temporal execution, in the admissions' fenced custody.
func constructEngineChildAuthority(_ context.Context, db databaseServices, auth controlAuth, manager *lease.Manager, owner func() *lease.Handle, p *controlPlanes) error {
	switch {
	case p.engineChildren == nil:
		return errors.New("engine child authority requires the engine child admissions (#7879)")
	case db.Temporal == nil:
		return errors.New("engine child authority requires the control Temporal client")
	case auth.GrantKeys == nil:
		return errors.New("engine child authority requires the credential-grant key")
	}
	backend, err := journalpg.New(journalpg.Config{Pool: db.Pools.Transaction, InstanceID: db.InstanceID, Lease: manager, LiveOwner: owner})
	if err != nil {
		return err
	}
	authority := childengine.FencedAuthority{Fenced: *p.engineChildren, Grants: auth.GrantKeys,
		Journal: func(ctx context.Context, runID string) (journal.RunReader, error) {
			return backend.OpenRun(ctx, journal.RunRef{ID: runID, ReadOnly: true})
		},
		Execution: func(ctx context.Context, runID string) (startfence.StartedExecution, error) {
			return startfence.DescribeStarted(ctx, db.Temporal, runID)
		}}
	if err := authority.Validate(); err != nil {
		return err
	}
	plane, err := enginechildplane.NewAuthority(authority)
	if err != nil {
		return err
	}
	p.EngineChildAuthority = httpapi.WithEngineChildAuthority(plane)
	p.engineChildAuthority = &authority
	return nil
}

// constructEngineChildProtocol serves the rest of the engine child protocol
// to engine workers (#8085 d): Finalize seals a child's result from its
// control-side journal; Park and Complete stay unavailable until the control
// scheduler is composed as the parent capacity plane.
func constructEngineChildProtocol(_ context.Context, db databaseServices, _ controlAuth, manager *lease.Manager, owner func() *lease.Handle, p *controlPlanes) error {
	if p.engineChildren == nil {
		return errors.New("engine child protocol requires the engine child admissions (#7879)")
	}
	backend, err := journalpg.New(journalpg.Config{Pool: db.Pools.Transaction, InstanceID: db.InstanceID, Lease: manager, LiveOwner: owner})
	if err != nil {
		return err
	}
	protocol := childengine.FencedProtocol{Fenced: *p.engineChildren,
		Journal: func(ctx context.Context, runID string) (journal.RunReader, error) {
			return backend.OpenRun(ctx, journal.RunRef{ID: runID, ReadOnly: true})
		}}
	if err := protocol.Validate(); err != nil {
		return err
	}
	plane, err := enginechildplane.NewProtocol(protocol)
	if err != nil {
		return err
	}
	p.EngineChildProtocol = httpapi.WithEngineChildProtocol(plane)
	return nil
}

// constructChildWorkflows serves the child-workflow routes (validate, start,
// status, resolve) to engine parents' stage pods (#8085 c). Each request
// re-resolves the parent's authority and checks the grant the engine child
// authority bound, so a grant whose attempt ended, or that was revoked,
// replaced or never bound, authorizes nothing.
func constructChildWorkflows(_ context.Context, _ databaseServices, _ controlAuth, _ *lease.Manager, _ func() *lease.Handle, p *controlPlanes) error {
	if p.engineChildAuthority == nil {
		return errors.New("child workflows require the engine child authority (#8085)")
	}
	p.ChildWorkflows = httpapi.WithChildWorkflowService(childengine.FencedChildWorkflows{Authority: *p.engineChildAuthority})
	return nil
}
