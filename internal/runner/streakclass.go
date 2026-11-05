package runner

import "github.com/goobers/goobers/internal/terminalhooks"

// TerminalFault is terminalhooks.TerminalFault: the classification lives in
// internal/terminalhooks so the cloud controller can use it without the local
// runner; these keep the runner's names.
type TerminalFault = terminalhooks.TerminalFault

const (
	// TerminalFaultUnclassified is terminalhooks.TerminalFaultUnclassified.
	TerminalFaultUnclassified = terminalhooks.TerminalFaultUnclassified
	// TerminalFaultItemInfrastructure is terminalhooks.TerminalFaultItemInfrastructure.
	TerminalFaultItemInfrastructure = terminalhooks.TerminalFaultItemInfrastructure
	// TerminalFaultSubstrate is terminalhooks.TerminalFaultSubstrate.
	TerminalFaultSubstrate = terminalhooks.TerminalFaultSubstrate
)

var (
	// JournaledTerminalFault is terminalhooks.JournaledTerminalFault.
	JournaledTerminalFault = terminalhooks.JournaledTerminalFault
	// EndedByCancellation is terminalhooks.EndedByCancellation.
	EndedByCancellation = terminalhooks.EndedByCancellation
)
