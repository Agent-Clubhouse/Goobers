package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/temporalcli"
)

func main() {
	output := flag.String("output", "", "destination path for the Temporal CLI")
	flag.Parse()
	if *output == "" {
		fmt.Fprintln(os.Stderr, "temporal CLI download: -output is required")
		os.Exit(2)
	}
	if err := temporalcli.Download(context.Background(), *output); err != nil {
		fmt.Fprintf(os.Stderr, "temporal CLI download: %v\n", err)
		os.Exit(1)
	}
}
