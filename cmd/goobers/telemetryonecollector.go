package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/telemetry/onecollector"
)

const telemetryOneCollectorHelp = "Usage: goobers telemetry onecollector --capabilities --json\n\n" +
	"Report protected-profile support without resolving credentials or sending data.\n"

func runTelemetryOneCollector(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "upload":
			return runOneCollectorUpload(args[1:], stdout, stderr)
		case "mcp":
			return runOneCollectorMCP(args[1:], stdout, stderr)
		}
	}
	fs := newCLIFlagSet("telemetry onecollector", flag.ContinueOnError)
	fs.SetOutput(stderr)
	capabilities := fs.Bool("capabilities", false, "report supported profile contract")
	asJSON := fs.Bool("json", false, "render capabilities as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || !*capabilities || !*asJSON {
		pf(stderr, "%s", telemetryOneCollectorHelp)
		return 2
	}
	if err := writeJSONLine(stdout, struct {
		Supported      bool `json:"supported"`
		ProfileVersion int  `json:"profile_version"`
	}{true, 1}); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	return 0
}

func configureOneCollectorProfile(path, root string, asJSON bool, stdout, stderr io.Writer) int {
	if !filepath.IsAbs(path) {
		pf(stderr, "error: --onecollector-profile-file requires an absolute path\n")
		return 2
	}
	path = filepath.Clean(path)
	if _, err := onecollector.LoadProfile(path); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		pf(stderr, "error: load config: %v\n", err)
		return 2
	}
	changed := cfg.Telemetry.OneCollector == nil
	if !changed && filepath.Clean(cfg.Telemetry.OneCollector.ProfileFile) != path {
		pf(stderr, "error: a different OneCollector profile is already configured\n")
		return 1
	}
	if changed {
		cfg.Telemetry.OneCollector = &instance.OneCollectorConfig{ProfileFile: path}
		if err := cfg.Validate(); err != nil {
			pf(stderr, "error: invalid instance configuration: %v\n", err)
			return 1
		}
		if err := runtimewiring.PrepareManualRoot(layout, stderr); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		doc, err := readYAMLNodeDocument(layout.ConfigFile())
		if err != nil {
			pf(stderr, "error: read config document: %v\n", err)
			return 2
		}
		rootNode := doc.Content[0]
		if rootNode.Kind != yaml.MappingNode {
			pf(stderr, "error: instance configuration must be a mapping\n")
			return 1
		}
		telemetryNode := yamlMapValue(rootNode, "telemetry")
		if telemetryNode == nil {
			telemetryNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			rootNode.Content = append(rootNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "telemetry"}, telemetryNode)
		}
		if telemetryNode.Kind != yaml.MappingNode || yamlMapValue(telemetryNode, "onecollector") != nil {
			pf(stderr, "error: cannot replace an existing telemetry profile or non-mapping configuration\n")
			return 1
		}
		profileNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "profile_file"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: path},
		}}
		telemetryNode.Content = append(telemetryNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "onecollector"}, profileNode)
		rendered, _, err := encodeYAMLNode(doc)
		if err != nil {
			pf(stderr, "error: encode config document: %v\n", err)
			return 2
		}
		info, err := os.Stat(layout.ConfigFile())
		if err != nil {
			pf(stderr, "error: stat config: %v\n", err)
			return 2
		}
		if err := journal.WriteFileAtomic(layout.ConfigFile(), rendered, info.Mode().Perm()); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	}
	if asJSON {
		if err := writeJSONLine(stdout, struct {
			Changed     bool   `json:"changed"`
			ProfileFile string `json:"profile_file"`
		}{changed, path}); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	} else {
		pf(stdout, "OneCollector profile configured; no data was sent\n")
	}
	return 0
}
