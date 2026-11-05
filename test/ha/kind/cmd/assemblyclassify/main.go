package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: assemblyclassify STATUS")
		os.Exit(2)
	}
	status, err := strconv.Atoi(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "assemblyclassify: invalid status")
		os.Exit(2)
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "assemblyclassify: %v\n", err)
		os.Exit(2)
	}
	output := string(input)
	fmt.Print(output)
	appendFile(os.Getenv("GITHUB_STEP_SUMMARY"), output)
	switch {
	case status == 0:
		appendFile(os.Getenv("GITHUB_OUTPUT"), "ready=true\n")
	case pendingNeedsFlag(output):
		fmt.Println("::error title=cloud assembly configuration drift::a compiled seam is pending because a deployed flag was not supplied")
		os.Exit(1)
	case strings.Contains(output, "\tpending\t"):
		appendFile(os.Getenv("GITHUB_OUTPUT"), "ready=false\n")
		fmt.Println("::warning title=cloud reference blocked::control assembly still has explicitly unimplemented seams; cloud deployment was not attempted")
	default:
		fmt.Println("::error title=cloud assembly check failed::all compiled seams must either report pending or compose successfully")
		os.Exit(status)
	}
}

func pendingNeedsFlag(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "\tpending\t") && strings.Contains(line, "\tneeds ") {
			return true
		}
	}
	return false
}

func appendFile(path, value string) {
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "assemblyclassify: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = file.Close() }()
	if _, err := io.WriteString(file, value); err != nil {
		fmt.Fprintf(os.Stderr, "assemblyclassify: %v\n", err)
		os.Exit(2)
	}
}
