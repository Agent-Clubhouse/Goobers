// Command fleetdiagnostics runs the synthetic two-company OTLP reference fixture.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/fleetdiagnostics"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := fleetdiagnostics.ReferenceFixture(ctx, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
