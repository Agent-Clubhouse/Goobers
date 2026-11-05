// Command decisioneval prints the System One vs LLM-judge comparison for a
// labelled fixture using the recorded (replay) answers it contains. It makes
// no network calls.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/decisioneval"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "decisioneval:", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("fixture", "", "labelled fixture JSON file")
	minN := flag.Int("min-samples", decisioneval.DefaultMinSamples, "smallest denominator that prints a percentage")
	repeats := flag.Int("repeats", 3, "runs per item for determinism")
	flag.Parse()
	if decisioneval.LiveEnabled(os.Getenv) {
		fmt.Fprintln(os.Stderr, decisioneval.LiveEnv+" is set, but this command only replays recorded answers; live arms are library-only")
	}
	f, err := decisioneval.LoadFixture(*path)
	if err != nil {
		return err
	}
	all := f.Items
	rep, err := decisioneval.Run(context.Background(), f, decisioneval.NewReplayDecider(all), decisioneval.NewReplayJudge(all),
		decisioneval.Options{MinSamples: *minN, Repeats: *repeats})
	if err != nil {
		return err
	}
	fmt.Print(rep.Render())
	return nil
}
