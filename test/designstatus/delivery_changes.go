package main

import "fmt"

// checkDeliveryChanges compares against the base tree as well as the new tree:
// deleting a tracking reference cannot hide a closing work item from the gate.
func checkDeliveryChanges(before, after []document, closing []string) []string {
	base, head := map[string]document{}, map[string]document{}
	for _, doc := range before {
		base[doc.Path] = doc
	}
	for _, doc := range after {
		head[doc.Path] = doc
	}
	paths := map[string]bool{}
	for path := range base {
		paths[path] = true
	}
	for path := range head {
		paths[path] = true
	}
	var problems []string
	for path := range paths {
		old, current := base[path], head[path]
		for _, ref := range closing {
			if !tracksDelivery(old, ref) && !tracksDelivery(current, ref) {
				continue
			}
			if current.Path == "" {
				problems = append(problems, fmt.Sprintf("%s: closing %s requires retaining and updating its design delivery record", path, ref))
				continue
			}
			if !contains(current.DeliveredBy, ref) {
				problems = append(problems, fmt.Sprintf("%s: closing %s requires adding it to Delivered-by", path, ref))
			}
			if contains(current.Remaining, ref) {
				problems = append(problems, fmt.Sprintf("%s: closing %s must remove it from Pending-delivery", path, ref))
			}
			if current.ScopeDelta == "" {
				problems = append(problems, fmt.Sprintf("%s: closing %s requires Scope-delta (use an explicit no-delta statement when all designed scope shipped)", path, ref))
			}
			if current.Verified == "" || current.Verified == old.Verified {
				problems = append(problems, fmt.Sprintf("%s: closing %s requires a refreshed Verified revision/date", path, ref))
			}
		}
	}
	return problems
}

func tracksDelivery(doc document, ref string) bool {
	return contains(doc.Tracking, ref) || contains(doc.Remaining, ref) || contains(doc.DeliveredBy, ref)
}
