package validate

import (
	"bytes"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// secretShapedInputs is the pattern net every candidate input literal is
// tested against. It is the SAME net the journal's boundary scrubber applies
// to anything a stage writes (journal.NewPatternScrubber), so an author's
// finding here names exactly the shapes the runtime would have had to redact
// downstream. The exact-value registry — the canary's mechanism — cannot help
// at author time: nothing has been minted yet when a config is validated.
var secretShapedInputs = journal.NewPatternScrubber()

// isSecretShaped reports whether the pattern net would redact any part of
// value, i.e. whether the literal is shaped like a credential.
func isSecretShaped(value string) bool {
	if value == "" {
		return false
	}
	raw := []byte(value)
	return !bytes.Equal(secretShapedInputs.Scrub(raw), raw)
}

// checkSecretShapedInputs reports SEC001 for every static stage input literal
// — including an experiment arm's variant overlay, which merges into the same
// envelope inputs blob — that is shaped like a credential.
//
// The finding names the workflow, the stage, and the input KEY, and never the
// value: a validation report is written to logs, CI annotations, and the JSON
// report, so echoing the suspected credential would leak it into three more
// places than the config already did.
func checkSecretShapedInputs(r *Report, w apiv1.Workflow, file string) {
	for _, task := range w.Spec.Tasks {
		for _, key := range secretShapedKeys(task.Inputs) {
			r.addWarning(WarningSecretShapedInput, file, w.Spec.Gaggle, "Workflow", w.Name,
				"task %q input %q is a secret-shaped literal; stage inputs are history-resident — they are merged into the invocation envelope and persisted verbatim in durable workflow history — so pass an opaque reference here and declare a credential capability for the value instead (#2931)",
				task.Name, key)
		}
		if task.Experiment == nil {
			continue
		}
		for _, arm := range task.Experiment.Arms {
			for _, key := range secretShapedKeys(arm.Variant) {
				r.addWarning(WarningSecretShapedInput, file, w.Spec.Gaggle, "Workflow", w.Name,
					"task %q experiment arm %q overlays input %q with a secret-shaped literal; stage inputs are history-resident — they are merged into the invocation envelope and persisted verbatim in durable workflow history — so pass an opaque reference here and declare a credential capability for the value instead (#2931)",
					task.Name, arm.Name, key)
			}
		}
	}
}

// secretShapedKeys returns the secret-shaped keys of inputs in sorted order so
// a config with more than one finding reports them deterministically.
func secretShapedKeys(inputs map[string]string) []string {
	var keys []string
	for key, value := range inputs {
		if isSecretShaped(value) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
