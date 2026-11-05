package chaos

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

//go:embed pending_refs.json
var pendingRefsJSON []byte

var pendingRef = regexp.MustCompile(`^pending #([0-9]+)$`)

// PendingRefs returns the checked-in state of every issue or PR a pending
// reference may cite ("open" is the only state that justifies a skip).
func PendingRefs() (map[string]string, error) {
	var refs map[string]string
	if err := json.Unmarshal(pendingRefsJSON, &refs); err != nil {
		return nil, fmt.Errorf("pending_refs.json: %w", err)
	}
	return refs, nil
}

// PendingNumber extracts N from "pending #N".
func PendingNumber(reason string) (string, bool) {
	m := pendingRef.FindStringSubmatch(reason)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// VerifyPendingOpen asks GitHub (via gh) whether every cited reference is
// still open. A merged PR or closed issue means the skip is stale: the
// scenario or invariant must be given a body and drop its Pending field.
func VerifyPendingOpen(ctx context.Context, exec Executor, refs []string) error {
	sort.Strings(refs)
	var stale []string
	for _, n := range refs {
		out, err := exec.Run(ctx, "", "gh", "api", "repos/Agent-Clubhouse/Goobers/issues/"+n, "--jq", ".state")
		if err != nil {
			return err
		}
		if state := strings.TrimSpace(out); state != "open" {
			stale = append(stale, fmt.Sprintf("#%s is %s", n, state))
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("stale pending references: %s", strings.Join(stale, ", "))
	}
	return nil
}
