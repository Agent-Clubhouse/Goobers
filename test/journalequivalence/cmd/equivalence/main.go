// Command equivalence checks that known journal divergences are still tracked.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/test/ha/chaos"
	"github.com/goobers/goobers/test/journalequivalence"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "verify-pending" {
		fmt.Fprintln(os.Stderr, "usage: equivalence verify-pending")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := journalequivalence.VerifyPending(ctx, chaos.OSExecutor{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("journal equivalence: all known divergences have open tracking issues")
}
