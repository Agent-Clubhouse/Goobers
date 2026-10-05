package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, execRunner{}))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, runner commandRunner) int {
	flags := flag.NewFlagSet("componentization-inventory", flag.ContinueOnError)
	flags.SetOutput(stderr)
	target := flags.String("target", "./cmd/goobers", "command package to inventory")
	goos := flags.String("goos", "", "GOOS override (default: go env GOOS)")
	goarch := flags.String("goarch", "", "GOARCH override (default: go env GOARCH)")
	tagsValue := flags.String("tags", "", "comma-separated Go build tags")
	output := flags.String("output", "-", "output file, or - for stdout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "error: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}

	tags := splitTags(*tagsValue)
	result, err := (discovery{runner: runner, goos: *goos, goarch: *goarch, tags: tags}).collect(ctx, *target)
	if err != nil {
		fmt.Fprintf(stderr, "error: inventory failed: %v\n", err)
		return 1
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "error: encode inventory: %v\n", err)
		return 1
	}
	data = append(data, '\n')
	if *output == "-" {
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintf(stderr, "error: write inventory: %v\n", err)
			return 1
		}
		return 0
	}
	if err := os.WriteFile(*output, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "error: write inventory %s: %v\n", *output, err)
		return 1
	}
	return 0
}

func splitTags(value string) []string {
	var tags []string
	for _, tag := range strings.Split(value, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return sortedUnique(tags)
}
