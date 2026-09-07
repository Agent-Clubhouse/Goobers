package validate

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func (ix *index) checkGooberDirectoryScope(r *Report, g apiv1.Goober, file string) {
	shared := strings.HasPrefix(file, "../goobers/")
	if shared {
		parts := strings.Split(file, "/")
		if len(parts) != 4 || parts[2] != g.Name {
			r.add(errorGooberGaggleReference, Error, file, "Goober", g.Name,
				"instance-shared goober must live directly in ../goobers/%s/", g.Name)
		}
	}
	if shared && g.Spec.Gaggle != "" {
		r.add(errorGooberGaggleReference, Error, file, "Goober", g.Name,
			"instance-shared goober must omit spec.gaggle; its directory defines its scope")
	}
	if !shared && g.Spec.Gaggle == "" {
		r.add(errorGooberGaggleReference, Error, file, "Goober", g.Name,
			"spec.gaggle is required outside the instance-shared ../goobers/<name>/ directory")
	}
}

func (ix *index) checkGooberDuplicate(r *Report, doc loadedDoc) {
	old, exists := ix.gooberFile[doc.name]
	if !exists {
		return
	}
	if strings.HasPrefix(old, "../goobers/") != strings.HasPrefix(doc.file, "../goobers/") {
		r.add(errorDuplicateDefinition, Error, doc.file, "Goober", doc.name,
			"instance-shared and gaggle-scoped goober name %q collide (%s and %s); shadowing is not supported",
			doc.name, old, doc.file)
		return
	}
	ix.dupCheck(r, doc, "Goober", doc.name, func() bool { return true })
}

// A shared persona's optional workflow list names workflows across gaggles;
// it does not manufacture an owning gaggle for the persona.
func (ix *index) sharedGooberWorkflowExists(gaggle, name string) bool {
	if gaggle != "" {
		return false
	}
	for identity := range ix.workflows {
		if identity.name == name {
			return true
		}
	}
	return false
}
