package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/goobers/goobers/internal/cloud/lease"
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
	{field: "EngineChildAdmissions", label: "engine child admissions", issue: "#7879", present: func(p controlPlanes) bool { return p.EngineChildAdmissions != nil }},
	{field: "Configuration", label: "admission guard", issue: "#7860", controller: true, present: func(p controlPlanes) bool { return p.Configuration != nil }, construct: constructConfiguration, requires: requiresConfigDirectory},
	{field: "Reader", issue: "#7811", present: func(p controlPlanes) bool { return p.Reader != nil }},
	{field: "Claims", issue: "#7808", present: func(p controlPlanes) bool { return p.Claims != nil }},
	{field: "Triggers", issue: "#7818", present: func(p controlPlanes) bool { return p.Triggers != nil }, construct: constructTriggers},
	{field: "Journal", issue: "#7818", controller: true, present: func(p controlPlanes) bool { return p.Journal != nil }, construct: constructJournal, requires: requiresObjectStore},
	{field: "RunJournal", issue: "#7818", present: func(p controlPlanes) bool { return p.RunJournal != nil }},
	{field: "Resolve", controller: true, issue: "#7818", present: func(p controlPlanes) bool { return p.Resolve != nil }},
	{field: "Epoch", controller: true, issue: "#7818", present: func(p controlPlanes) bool { return p.Epoch != nil }},
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
