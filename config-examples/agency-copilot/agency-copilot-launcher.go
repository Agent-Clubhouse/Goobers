package main

import (
	"fmt"
	"os"
	"os/exec"
)

const launcherContractFlag = "--goobers-launcher-contract"

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == launcherContractFlag {
		fmt.Println(`{"version":1,"sessionMode":"adapter-managed"}`)
		return
	}

	program := commandFromEnv("AGENCY_BIN", "agency")
	forwarded := append([]string{"copilot"}, args...)
	if useDirectCopilot(args) {
		program = commandFromEnv("COPILOT_BIN", "copilot")
		forwarded = args
	}

	command := exec.Command(program, forwarded...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = os.Environ()
	if err := command.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			os.Exit(exitError.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func commandFromEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func useDirectCopilot(args []string) bool {
	headless := false
	stdio := false
	for _, arg := range args {
		if arg == "--version" || arg == "version" {
			return true
		}
		headless = headless || arg == "--headless"
		stdio = stdio || arg == "--stdio"
	}
	return headless && stdio
}
