// Command decisionscope prints the reviewer-finding scope shadow report from a
// JSON-lines file of decisiongate.ScopeRecord values. It makes no network
// calls and refuses percentages below a minimum sample.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/decisiongate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "decisionscope:", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("records", "", "JSON-lines scope shadow records")
	minN := flag.Int("min-sample", decisiongate.DefaultScopeMinSample, "smallest denominator that prints a percentage")
	flag.Parse()
	if *path == "" {
		return errors.New("-records is required")
	}
	f, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	recs, err := decisiongate.ReadScopeRecords(f)
	if err != nil {
		return err
	}
	fmt.Print(decisiongate.NewScopeTally(recs).Render(*minN))
	return nil
}
