package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
)

type serviceLifecycleSpec[M any, S any] struct {
	Name                string
	ParseRoot           func([]string, io.Writer) (string, bool)
	NewManager          func(string) (M, error)
	Check               func(M, io.Writer) int
	Before              func(string, io.Writer) int
	Action              func(context.Context, string, M) (S, error)
	ErrorPrefix         string
	Error               func(string, error, io.Writer) int
	NotInstalled        func(error) bool
	NotInstalledMessage string
	NotInstalledExit    int
	Success             func(io.Writer, S)
}

func runServiceLifecycleCommand[M any, S any](args []string, stdout, stderr io.Writer, spec serviceLifecycleSpec[M, S]) int {
	parseRoot := spec.ParseRoot
	if parseRoot == nil {
		parseRoot = func(args []string, stderr io.Writer) (string, bool) {
			return parseServiceRoot(spec.Name, spec.Name, args, stderr)
		}
	}
	root, ok := parseRoot(args, stderr)
	if !ok {
		return 2
	}
	manager, err := spec.NewManager(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if spec.Check != nil {
		if exit := spec.Check(manager, stderr); exit != 0 {
			return exit
		}
	}
	if spec.Before != nil {
		if exit := spec.Before(root, stderr); exit != 0 {
			return exit
		}
	}
	status, err := spec.Action(context.Background(), root, manager)
	if err != nil {
		if spec.NotInstalled != nil && spec.NotInstalled(err) {
			pln(stdout, spec.NotInstalledMessage)
			return spec.NotInstalledExit
		}
		if spec.Error != nil {
			return spec.Error(root, err, stderr)
		}
		pf(stderr, "error: %s: %v\n", spec.ErrorPrefix, err)
		return 1
	}
	spec.Success(stdout, status)
	return 0
}

type serviceStatusSpec[M any, S any] struct {
	Name              string
	NewManager        func(string) (M, error)
	Status            func(context.Context, M) (S, error)
	Transform         func(string, S) S
	StatusErrorPrefix string
	EncodeErrorPrefix string
	Render            func(string, io.Writer, S)
	Exit              func(S) int
}

func runServiceStatusCommand[M any, S any](args []string, stdout, stderr io.Writer, spec serviceStatusSpec[M, S]) int {
	fs := newCLIFlagSet(spec.Name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "render status as JSON")
	fs.Usage = helpUsage(stderr, spec.Name)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := serviceRootFromFlagSet(fs, stderr)
	if !ok {
		return 2
	}
	manager, err := spec.NewManager(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	status, err := spec.Status(context.Background(), manager)
	if err != nil {
		pf(stderr, "error: %s: %v\n", spec.StatusErrorPrefix, err)
		return 1
	}
	if spec.Transform != nil {
		status = spec.Transform(root, status)
	}
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(status); err != nil {
			pf(stderr, "error: %s: %v\n", spec.EncodeErrorPrefix, err)
			return 1
		}
	} else {
		spec.Render(root, stdout, status)
	}
	return spec.Exit(status)
}
