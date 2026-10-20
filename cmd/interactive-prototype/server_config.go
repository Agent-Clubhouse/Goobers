package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type prototypeConfig struct {
	listen, stateDir, source, assets, binary, actorsFlag, configDir string
	address, port                                                   string
	localLogin                                                      bool
}

func parsePrototypeConfig() (prototypeConfig, error) {
	listen := flag.String("listen", "127.0.0.1:8097", "loopback HTTP address")
	stateDir := flag.String("state-dir", "", "private durable state directory outside the source repository (required)")
	source := flag.String("source", "samples/interactive-investigation", "trusted local repository snapshot source")
	assets := flag.String("assets", "internal/portalassets/dist", "built portal directory")
	binary := flag.String("goobers-bin", ".tools/goobers", "native Goobers CLI for Helper workflow operations")
	localLogin := flag.Bool("trusted-local-model-login", false, "explicitly use existing trusted local Codex login; never provider delegation")
	actorsFlag := flag.String("local-actors", "Mason,Alex", "controlled local personas for isolation demonstrations; not real remote authentication")
	configDir := flag.String("config-dir", "samples/interactive-config", "normal Goobers config tree declaring DSL 3.0 interactive surfaces")
	flag.Parse()
	if *stateDir == "" {
		return prototypeConfig{}, fmt.Errorf("--state-dir is required; use a private directory outside git")
	}
	address, port, err := net.SplitHostPort(*listen)
	if err != nil {
		return prototypeConfig{}, err
	}
	ip := net.ParseIP(address)
	if ip == nil || !ip.IsLoopback() {
		return prototypeConfig{}, fmt.Errorf("prototype requires a literal loopback --listen address")
	}
	*stateDir, err = filepath.Abs(*stateDir)
	if err != nil {
		return prototypeConfig{}, err
	}
	*source, err = filepath.Abs(*source)
	if err != nil {
		return prototypeConfig{}, err
	}
	*assets, err = filepath.Abs(*assets)
	if err != nil {
		return prototypeConfig{}, err
	}
	*binary, err = filepath.Abs(*binary)
	if err != nil {
		return prototypeConfig{}, err
	}
	if relative, err := filepath.Rel(*source, *stateDir); err == nil && (relative == "." || !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != "..") {
		return prototypeConfig{}, fmt.Errorf("state directory must not be inside snapshot source")
	}
	if err := os.MkdirAll(*stateDir, 0700); err != nil {
		return prototypeConfig{}, err
	}
	if err := os.Chmod(*stateDir, 0700); err != nil {
		return prototypeConfig{}, err
	}
	for path := *stateDir; ; path = filepath.Dir(path) {
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			return prototypeConfig{}, fmt.Errorf("--state-dir must be outside any git checkout")
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	return prototypeConfig{listen: *listen, stateDir: *stateDir, source: *source, assets: *assets, binary: *binary, actorsFlag: *actorsFlag, configDir: *configDir, address: address, port: port, localLogin: *localLogin}, nil
}
