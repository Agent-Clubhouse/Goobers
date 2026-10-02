package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/testgit"
)

// The native fixture subcommand is small real work, never a pressure injector.
// Its timeout leaves room for cleanup before the workflow's 60-second deadline.
func runFixture(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) != 1 || (args[0] != "success" && args[0] != "failure") {
		_, _ = fmt.Fprintln(stderr, "fixture requires success or failure")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	if err := churnFixture(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if err := (wallClock{}).Wait(ctx, 3*time.Second); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	result := "{\"fixture\":\"complete\",\"integrity\":\"unapproved\"}\n"
	code := 0
	if args[0] == "failure" {
		result = "{\"errorCode\":\"soak_fixture_failure\",\"errorMessage\":\"soak_fixture_failure\",\"errorRetryable\":false,\"integrity\":\"unapproved\"}\n"
		code = 1
	}
	if err := os.WriteFile("result.json", []byte(result), 0o600); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	return code
}

func churnFixture(ctx context.Context) (err error) {
	// Keep all temporary state within the disposable stage workspace, including
	// if the executor must kill this process before deferred cleanup can run.
	scratch, err := os.MkdirTemp(".", ".soak-fixture-")
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(scratch); err == nil {
			err = cleanupErr
		}
	}()
	if err := fixtureGit(ctx, scratch, nil, "-c", "init.templateDir=", "init", "-q"); err != nil {
		return err
	}
	for i := range 24 {
		path := filepath.Join(scratch, fmt.Sprintf("file-%d", i))
		if err := os.WriteFile(path, fmt.Appendf(nil, "fixture file %d\n", i), 0o600); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		// The subprocess receives a real open file descriptor as stdin.
		err = fixtureGit(ctx, scratch, file, "hash-object", "--stdin")
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, args := range [][]string{
		{"add", "."},
		{"-c", "user.name=soak", "-c", "user.email=soak@invalid", "commit", "-qm", "fixture"},
		{"fsck", "--no-reflogs"},
	} {
		if err := fixtureGit(ctx, scratch, nil, args...); err != nil {
			return err
		}
	}
	return nil
}

func fixtureGit(ctx context.Context, dir string, stdin *os.File, args ...string) error {
	cmd := testgit.CommandContext(ctx, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.WaitDelay = time.Second
	// Exclude ambient repositories, templates, hooks, signing, filters and
	// command-line configuration. Only this newly created repository is used.
	cmd.Env = nil
	for _, value := range cleanEnv() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fixture git %v: %w: %s", args, err, output)
	}
	return nil
}
