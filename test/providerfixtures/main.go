// Command providerfixtures refreshes and checks the recorded GitHub provider
// contract fixture used by the reporting-only drift workflow.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/providerfixture"
)

const tokenEnvironment = "GOOBERS_PROVIDER_FIXTURE_TOKEN"

type refreshFunc func(context.Context, providerfixture.RefreshConfig) (providerfixture.Fixture, error)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	return runWithRefresh(args, getenv, stdout, stderr, providerfixture.Refresh)
}

func runWithRefresh(args []string, getenv func(string) string, stdout, stderr io.Writer, refresh refreshFunc) int {
	if len(args) == 0 {
		if _, err := fmt.Fprintln(stderr, "usage: providerfixtures <refresh|contract|drift> [flags]"); err != nil {
			return 1
		}
		return 1
	}
	var err error
	switch args[0] {
	case "refresh":
		err = runRefresh(args[1:], getenv, stdout, stderr, refresh)
	case "contract":
		err = runContract(args[1:], stdout, stderr)
	case "drift":
		err = runDrift(args[1:], stdout, stderr)
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err == nil {
		return 0
	}
	if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
		return 1
	}
	return exitCode(err)
}

func exitCode(err error) int {
	switch {
	case errors.Is(err, providerfixture.ErrContractAssertion):
		return 2
	case errors.Is(err, providerfixture.ErrFixtureDrift):
		return 3
	default:
		return 1
	}
}

func runRefresh(args []string, getenv func(string) string, stdout, stderr io.Writer, refresh refreshFunc) error {
	flags := flag.NewFlagSet("refresh", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repository := flags.String("repository", "", "designated fixture repository in owner/name form")
	issue := flags.String("issue", "", "stable fixture issue number")
	pullRequest := flags.String("pull-request", "", "stable fixture pull request number")
	output := flags.String("output", "", "candidate fixture output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	owner, name, ok := strings.Cut(*repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("-repository must use owner/name form")
	}
	if (*issue == "") == (*pullRequest == "") {
		return fmt.Errorf("exactly one of -issue or -pull-request is required")
	}
	if *output == "" {
		return fmt.Errorf("-output is required")
	}
	token := strings.TrimSpace(getenv(tokenEnvironment))
	if token == "" {
		return fmt.Errorf("%s is required; provision the dedicated fixture credential before making live calls", tokenEnvironment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, err := refresh(ctx, providerfixture.RefreshConfig{
		Repository:  providerfixture.Repository{Owner: owner, Name: name},
		Issue:       *issue,
		PullRequest: *pullRequest,
		Token:       token,
	})
	if err != nil {
		return fmt.Errorf("refresh provider fixture: %w", err)
	}
	if err := providerfixture.Write(*output, fixture); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "wrote normalized provider fixture candidate to %s\n", *output)
	return err
}

func runContract(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("contract", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("fixture", "", "normalized fixture to replay")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("-fixture is required")
	}
	fixture, err := providerfixture.Read(*path)
	if err != nil {
		return fmt.Errorf("%w: read fixture: %w", providerfixture.ErrContractAssertion, err)
	}
	if err := providerfixture.CheckContract(context.Background(), fixture); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "provider contract assertions passed")
	return err
}

func runDrift(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("drift", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baselinePath := flags.String("baseline", "", "checked-in normalized fixture")
	candidatePath := flags.String("candidate", "", "fresh normalized fixture")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *baselinePath == "" || *candidatePath == "" {
		return fmt.Errorf("-baseline and -candidate are required")
	}
	baseline, err := providerfixture.Read(*baselinePath)
	if err != nil {
		return fmt.Errorf("read baseline: %w", err)
	}
	candidate, err := providerfixture.Read(*candidatePath)
	if err != nil {
		return fmt.Errorf("read candidate: %w", err)
	}
	if err := providerfixture.CheckDrift(baseline, candidate); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "normalized provider fixture matches the checked-in baseline")
	return err
}
