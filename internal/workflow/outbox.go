package workflow

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// CheckOutbox reports outbox declarations that cannot survive artifact export
// (#3662): an empty, absolute, or workspace-escaping outbox entry, and a
// mirror root that is neither absolute nor home-relative. Both are lexical
// properties of the document, so the rule is version-agnostic and lives in the
// router rather than in any interpreter.
//
// Catching them here — at config load and compile — keeps a misconfiguration
// from turning otherwise successful stage work into a runtime export failure
// after the task has already done its work.
func CheckOutbox(def Definition) []string {
	var problems []string
	if root := def.Spec.OutboxMirrorPath; root != "" {
		if err := apiv1.ValidateOutboxMirrorRoot(root); err != nil {
			problems = append(problems, fmt.Sprintf("spec.outboxMirrorPath: %v", err))
		}
	}
	for _, task := range def.Spec.Tasks {
		for i, entry := range task.Outbox {
			if err := apiv1.ValidateOutboxPath(entry); err != nil {
				problems = append(problems, fmt.Sprintf("task %q outbox[%d]: %v", task.Name, i, err))
			}
		}
		// A task mirror root equal to the workflow's is the resolved default
		// Compile fills in, already reported above.
		root := task.OutboxMirrorPath
		if root == "" || root == def.Spec.OutboxMirrorPath {
			continue
		}
		if err := apiv1.ValidateOutboxMirrorRoot(root); err != nil {
			problems = append(problems, fmt.Sprintf("task %q outboxMirrorPath: %v", task.Name, err))
		}
	}
	return problems
}
