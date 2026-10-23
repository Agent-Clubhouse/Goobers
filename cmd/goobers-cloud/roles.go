package main

import (
	"io"
)

const (
	apiHelp = `Usage: goobers-cloud api

Serve the cloud control-plane API. The P1 skeleton reserves this role while
the extracted API host is wired into the cloud composition root.
`
	controllerHelp = `Usage: goobers-cloud controller

Run cloud control-plane reconciliation. The P1 skeleton reserves this role
while the extracted controller host is wired into the cloud composition root.
`
	migrateHelp = `Usage: goobers-cloud migrate

Apply cloud data-store migrations. The P1 skeleton reserves this role while
the migration runner is wired into the cloud composition root.
`
)

func runAPIRole(args []string, stdout, stderr io.Writer) int {
	return runReservedRole("api", apiHelp, args, stdout, stderr)
}

func runControllerRole(args []string, stdout, stderr io.Writer) int {
	return runReservedRole("controller", controllerHelp, args, stdout, stderr)
}

func runMigrateRole(args []string, stdout, stderr io.Writer) int {
	return runReservedRole("migrate", migrateHelp, args, stdout, stderr)
}

func runReservedRole(role, help string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		write(stdout, help)
		return 0
	}
	if len(args) != 0 {
		write(stderr, help)
		return 2
	}
	writef(stderr, "goobers-cloud %s: role wiring is not available in the P1 skeleton\n", role)
	return 1
}

func runWorkerRole(args []string, stdout, stderr io.Writer) int {
	return runWorker(args, stdout, stderr)
}
