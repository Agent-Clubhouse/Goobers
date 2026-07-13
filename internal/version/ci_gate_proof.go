package version

// ciGateProof exists only to demonstrate the CI gate flipping red→green.
// Not merged; lives on the throwaway proof branch.
func ciGateProof() string {
	return "green"
}

var _ = ciGateProof
