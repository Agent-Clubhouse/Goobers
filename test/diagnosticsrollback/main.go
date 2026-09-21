// Command diagnosticsrollback exercises actual pinned prior-release readers and
// writers against current diagnostics fixtures, then rebuilds with current code.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// v0.4.1 peeled commit; Git archive provenance records the commit, not its
// annotated tag object e24c075a12b1188771e5fc2246108fc24cfa29b6.
const priorRevision = "74f44963a0a357418f5097da1f6ca212117e23a3"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	root, err := repositoryRoot(ctx)
	if err != nil {
		return err
	}
	if err := command(ctx, root, nil, "git", "cat-file", "-e", priorRevision+"^{commit}"); err != nil {
		if err := command(ctx, root, nil, "git", "fetch", "--no-tags", "origin", priorRevision); err != nil {
			return err
		}
	}
	scratch, err := os.MkdirTemp("", "goobers-diagnostics-rollback-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup rollback fixture:", err)
		}
	}()
	prior, fixture := filepath.Join(scratch, "prior"), filepath.Join(scratch, "fixture")
	for _, dir := range []string{prior, fixture} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
	}
	if err := extractRelease(ctx, root, prior); err != nil {
		return err
	}
	if err := sameFiles(filepath.Join(root, "internal/readmodel/schema.go"), filepath.Join(prior, "internal/readmodel/schema.go")); err != nil {
		return err
	}
	for _, pkg := range []string{"readmodel", "instance"} {
		name := "readmodel"
		if pkg == "instance" {
			name = "config"
		}
		data, err := os.ReadFile(filepath.Join(root, "testdata/rollback", name+"_test.go.txt"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(prior, "internal", pkg, "diagnostics_rollback_test.go"), data, 0600); err != nil {
			return err
		}
	}
	env := []string{"GOOBERS_ROLLBACK_FIXTURE=" + fixture}
	if err := fixtureTest(ctx, root, env, "./internal/readmodel", "TestDiagnosticsRollbackPrepare", true); err != nil {
		return err
	}
	if err := fixtureTest(ctx, root, env, "./internal/instance", "TestDiagnosticsRollbackConfigPrepare", true); err != nil {
		return err
	}
	before, err := schemaSnapshot(filepath.Join(fixture, "read.db"))
	if err != nil {
		return err
	}
	if err := command(ctx, prior, env, "go", "mod", "download"); err != nil {
		return err
	}
	if err := fixtureTest(ctx, prior, env, "./internal/readmodel", "TestDiagnosticsPriorReleaseReadWrite", false); err != nil {
		return err
	}
	if err := fixtureTest(ctx, prior, env, "./internal/instance", "TestDiagnosticsRollbackPriorConfig", false); err != nil {
		return err
	}
	if err := checkSchema(fixture, before); err != nil {
		return err
	}
	for _, step := range []struct{ pkg, name string }{
		{"./cmd/goobers", "TestDiagnosticsRollbackCLIRebuild"},
		{"./internal/readmodel", "TestDiagnosticsRollbackRestore"},
		{"./internal/instance", "TestDiagnosticsRollbackConfigVerify"},
	} {
		if err := fixtureTest(ctx, root, env, step.pkg, step.name, true); err != nil {
			return err
		}
	}
	return checkSchema(fixture, before)
}

func fixtureTest(ctx context.Context, dir string, env []string, pkg, name string, current bool) error {
	args := []string{"test"}
	if current {
		args = append(args, "-tags", "rollbackcompat")
	}
	args = append(args, pkg, "-run", "^"+name+"$", "-count=1", "-timeout=3m", "-v")
	return command(ctx, dir, env, "go", args...)
}

func command(ctx context.Context, dir string, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

func repositoryRoot(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(out)), nil
}

func sameFiles(first, second string) error {
	a, err := os.ReadFile(first)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(second)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return fmt.Errorf("SQLite schema source differs from pinned release: %s", priorRevision)
	}
	return nil
}

func schemaSnapshot(path string) ([]byte, error) {
	// No SQLite CLI/Python dependency; use the same driver as the application.
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	uri := url.URL{Scheme: "file", Path: slashed, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, "SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records [][4]sql.NullString
	for rows.Next() {
		var row [4]sql.NullString
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
			return nil, err
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(records)
}

func checkSchema(fixture string, want []byte) error {
	got, err := schemaSnapshot(filepath.Join(fixture, "read.db"))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("actual SQLite DDL changed across rollback/rebuild")
	}
	return nil
}

func extractRelease(ctx context.Context, root, destination string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "archive", priorRevision)
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 5 * time.Second
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extractArchive(pipe, destination)
	if extractErr == nil {
		_, extractErr = io.Copy(io.Discard, pipe)
	}
	if extractErr != nil {
		cancel()
		_ = pipe.Close()
	}
	waitErr := cmd.Wait()
	return errors.Join(extractErr, waitErr)
}

// The pinned tree contains only directories and regular files. Reject links,
// traversal and unexpected entry kinds rather than widening extraction policy.
func extractArchive(input io.Reader, destination string) error {
	reader := tar.NewReader(input)
	var total int64
	for count := 0; count < 100000; count++ {
		entry, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// git archive emits this global provenance record before file entries.
		// It has no extraction target and must not change path/link policy.
		if entry.Typeflag == tar.TypeXGlobalHeader {
			if count != 0 || entry.Name != "pax_global_header" || len(entry.PAXRecords) != 1 || entry.PAXRecords["comment"] != priorRevision {
				return errors.New("unexpected prior release global PAX metadata")
			}
			continue
		}
		name := filepath.FromSlash(entry.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("unsafe archive path %q", entry.Name)
		}
		path := filepath.Join(destination, name)
		switch entry.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			if entry.Size < 0 || entry.Size > 32<<20 || total+entry.Size > 512<<20 {
				return errors.New("prior release archive exceeds bounds")
			}
			total += entry.Size
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, reader, entry.Size)
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry %q type %d", entry.Name, entry.Typeflag)
		}
	}
	return errors.New("prior release archive exceeds entry bound")
}
