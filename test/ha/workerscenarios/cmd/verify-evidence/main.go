package main

import (
	"fmt"
	"os"

	"github.com/goobers/goobers/test/ha/workerscenarios"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: verify-evidence PATH SCENARIO...")
		os.Exit(2)
	}
	file, err := os.Open(os.Args[1])
	if err == nil {
		defer func() { _ = file.Close() }()
		err = workerscenarios.RequirePassedEvidence(file, os.Args[2:]...)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
