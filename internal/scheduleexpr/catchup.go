package scheduleexpr

import "fmt"

// ValidateCatchUp validates the workflow-wide policy shared by DSL compilers.
func ValidateCatchUp(policy string, declarations int) error {
	switch policy {
	case "", "coalesce":
		return nil
	case "all":
		if declarations >= 1 && declarations <= 1000 {
			return nil
		}
		return fmt.Errorf("scheduleCatchUp all requires 1..1000 schedule declarations (got %d)", declarations)
	default:
		return fmt.Errorf("scheduleCatchUp must be coalesce or all (got %q)", policy)
	}
}
