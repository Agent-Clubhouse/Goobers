// Package workitemtarget defines the bounded item identity shared by trigger
// transport, durable acceptance and backlog selection.
package workitemtarget

// ValidID accepts numeric provider identifiers without ambiguous spellings.
func ValidID(value string) bool {
	if len(value) == 0 || len(value) > 15 || value[0] == '0' {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
