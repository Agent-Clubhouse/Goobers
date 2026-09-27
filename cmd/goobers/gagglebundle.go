package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gagglebundle"
	"github.com/goobers/goobers/internal/instance"
)

const gaggleHelp = "Usage: goobers gaggle export|import [flags]\n\n" +
	"Export or import a sanitized, portable gaggle bundle. Bundles contain only\n" +
	"declarative gaggle, workflow, stage, Goober, instruction, skill, repository\n" +
	"reference, and provenance data. Structured credentials and runtime state are\n" +
	"excluded; companion text with recognized credential or local-path shapes is refused.\n"

const gaggleExportHelp = "Usage: goobers gaggle export [--output <file>] <gaggle> [path]\n\n" +
	"Validate the source configuration and write a deterministic JSON bundle.\n" +
	"The digest is stable for unchanged sanitized definitions; exportedAt is not\n" +
	"part of the digest. Without --output, write the bundle to stdout.\n"

const gaggleImportHelp = "Usage: goobers gaggle import --name <destination-name> <bundle-file> [path]\n\n" +
	"Validate the complete bundle and destination repository authorizations before\n" +
	"atomically creating a new gaggle. The source remains unchanged. Any schema,\n" +
	"digest, reference, name-conflict, authorization, validation, or write failure\n" +
	"leaves the destination configuration unchanged.\n"

func runGaggle(args []string, _ io.Writer, stderr io.Writer) int {
	pf(stderr, "%s", gaggleHelp)
	return 2
}

func runGaggleExport(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("gaggle export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	output := fs.String("output", "", "write the bundle atomically to this file instead of stdout")
	fs.Usage = helpUsage(stderr, "gaggle export")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	root, err := gaggleBundleInstanceRoot(fs.Arg(1), fs.NArg() == 2)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	bundle, err := gagglebundle.Export(instance.NewLayout(root).ConfigDir(), fs.Arg(0), time.Now())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	data, err := gagglebundle.MarshalJSON(bundle)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if *output == "" {
		_, _ = stdout.Write(data)
		return 0
	}
	path, err := filepath.Abs(*output)
	if err != nil {
		pf(stderr, "error: resolve output %s: %v\n", *output, err)
		return 2
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		pf(stderr, "error: create output directory: %v\n", err)
		return 1
	}
	if err := writeWorkflowSourceAtomically(path, data, 0o644); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "exported %s (%s) to %s\n", bundle.Source.Name, bundle.Digest, path)
	return 0
}

func runGaggleImport(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("gaggle import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "destination gaggle name")
	fs.Usage = helpUsage(stderr, "gaggle import")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 || strings.TrimSpace(*name) == "" {
		fs.Usage()
		return 2
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		pf(stderr, "error: read bundle: %v\n", err)
		return 1
	}
	var bundle apiv1.GaggleBundle
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		pf(stderr, "error: decode bundle: %v\n", err)
		return 1
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("bundle file must contain one JSON object")
		}
		pf(stderr, "error: decode bundle: %v\n", err)
		return 1
	}
	root, err := gaggleBundleInstanceRoot(fs.Arg(1), fs.NArg() == 2)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	swap, err := gagglebundle.PrepareImport(instance.NewLayout(root), *name, bundle)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := swap.Commit(); err != nil {
		pf(stderr, "error: commit imported gaggle: %v\n", err)
		return 1
	}
	pf(stdout, "imported %s from %s (%s)\n", *name, bundle.Source.Name, bundle.Digest)
	return 0
}

func gaggleBundleInstanceRoot(path string, supplied bool) (string, error) {
	if !supplied {
		path = "."
	}
	start, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", path, err)
	}
	info, err := os.Stat(start)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", start, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", start)
	}
	return findInstanceRoot(start)
}
