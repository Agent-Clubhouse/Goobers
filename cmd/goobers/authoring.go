package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/authoring"
	"github.com/goobers/goobers/internal/supportmatrix"
	buildversion "github.com/goobers/goobers/internal/version"
)

const (
	schemaOutputVersion  = "goobers.dev/schema/v1"
	explainOutputVersion = "goobers.dev/explain/v1"
)

const schemaHelp = "Usage: goobers schema [--human] <kind>\n" +
	"       goobers schema --list [--human]\n\n" +
	"Emit a canonical JSON Schema embedded in this build, or list every available\n" +
	"schema kind. JSON is the default and includes the build version, commit, and\n" +
	"DSL version. --human prints a labeled terminal rendering. No network lookup\n" +
	"or fallback to another release is performed. Exit codes: 0 = OK, 1 = unknown\n" +
	"kind or output error, 2 = usage error.\n"

const explainHelp = "Usage: goobers explain [--human] <selector>\n\n" +
	"Project field guidance from the embedded schema and feature registries using\n" +
	"a dotted or slash-\n" +
	"separated selector such as goober.spec.capabilities or\n" +
	"workflow/spec/gates[]/evaluator. Array elements use []. Output includes the\n" +
	"field purpose, type, allowed values, lifecycle, and a schema-grounded example.\n" +
	"JSON is the default; --human prints a terminal rendering. Exit codes: 0 = OK,\n" +
	"1 = unknown selector or output error, 2 = usage error.\n"

type authoringStamp struct {
	SchemaVersion string `json:"schemaVersion"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	DSLVersion    string `json:"dslVersion"`
}

type schemaListOutput struct {
	authoringStamp
	Kinds []string `json:"kinds"`
}

type schemaDocumentOutput struct {
	authoringStamp
	Kind string `json:"kind"`
}

type explainOutput struct {
	authoringStamp
	authoring.Explanation
}

func runSchema(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	list := fs.Bool("list", false, "list every embedded schema kind")
	human := fs.Bool("human", false, "emit a human-readable rendering")
	fs.Usage = helpUsage(stderr, "schema")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*list && fs.NArg() != 0) || (!*list && fs.NArg() != 1) {
		fs.Usage()
		return 2
	}

	stamp := newAuthoringStamp(schemaOutputVersion)
	if *list {
		kinds := schemas.Kinds()
		if *human {
			writeSchemaKinds(stdout, stamp, kinds)
			return 0
		}
		if err := encodeSchemaJSON(stdout, schemas.SchemaOutput, schemaListOutput{authoringStamp: stamp, Kinds: kinds}); err != nil {
			pf(stderr, "error: encode schema list: %v\n", err)
			return 1
		}
		return 0
	}

	entry, ok := schemas.Lookup(fs.Arg(0))
	if !ok {
		pf(stderr, "error: unknown schema kind %q\n", fs.Arg(0))
		return 1
	}
	raw, err := schemas.FS.ReadFile(entry.File)
	if err != nil {
		pf(stderr, "error: read embedded schema %q: %v\n", entry.Kind, err)
		return 1
	}
	if *human {
		pf(stdout, "Schema: %s\nVersion: %s\nCommit: %s\nDSL version: %s\n\n",
			entry.Kind, stamp.Version, stamp.Commit, stamp.DSLVersion)
		if _, err := stdout.Write(raw); err != nil {
			pf(stderr, "error: write schema %q: %v\n", entry.Kind, err)
			return 1
		}
		return 0
	}
	output := schemaDocumentOutput{authoringStamp: stamp, Kind: entry.Kind}
	encoded, err := marshalSchemaDocumentJSON(output, raw)
	if err != nil {
		pf(stderr, "error: encode schema %q: %v\n", entry.Kind, err)
		return 1
	}
	if _, err := stdout.Write(encoded); err != nil {
		pf(stderr, "error: write schema %q: %v\n", entry.Kind, err)
		return 1
	}
	return 0
}

func runExplain(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	human := fs.Bool("human", false, "emit a human-readable rendering")
	fs.Usage = helpUsage(stderr, "explain")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	explanation, err := authoring.Explain(fs.Arg(0))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	output := explainOutput{
		authoringStamp: newAuthoringStamp(explainOutputVersion),
		Explanation:    explanation,
	}
	if *human {
		writeExplanation(stdout, output)
		return 0
	}
	if err := encodeSchemaJSON(stdout, schemas.ExplainOutput, output); err != nil {
		pf(stderr, "error: encode explanation: %v\n", err)
		return 1
	}
	return 0
}

func newAuthoringStamp(schemaVersion string) authoringStamp {
	info := buildversion.Get()
	// The stamp tells an author which language version this output describes,
	// so it must name the newest SUPPORTED version: CurrentDSLVersion is
	// dropped (#3507), and stamping it would point authors at a version that
	// no longer loads (#3565).
	dslVersion, ok := supportmatrix.GetDSL().NewestSupported()
	if !ok {
		// No supported version declared at all — a matrix state the support
		// policy rejects. Fall back to the copy-forward version rather than
		// stamp an empty one.
		dslVersion = supportmatrix.NextDSLVersion
	}
	return authoringStamp{
		SchemaVersion: schemaVersion,
		Version:       info.Version,
		Commit:        info.Commit,
		DSLVersion:    dslVersion,
	}
}

func marshalSchemaDocumentJSON(output schemaDocumentOutput, raw []byte) ([]byte, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("embedded schema is not valid JSON")
	}
	prefix, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	if len(prefix) == 0 || prefix[len(prefix)-1] != '}' {
		return nil, fmt.Errorf("invalid schema envelope")
	}
	encoded := make([]byte, 0, len(prefix)+len(raw)+12)
	encoded = append(encoded, prefix[:len(prefix)-1]...)
	encoded = append(encoded, []byte(`,"schema":`)...)
	encoded = append(encoded, raw...)
	encoded = append(encoded, '}', '\n')
	if err := validateSchemaJSON(schemas.SchemaOutput, encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func writeSchemaKinds(w io.Writer, stamp authoringStamp, kinds []string) {
	pf(w, "Version: %s\nCommit: %s\nDSL version: %s\n\n", stamp.Version, stamp.Commit, stamp.DSLVersion)
	for _, kind := range kinds {
		pln(w, kind)
	}
}

func writeExplanation(w io.Writer, output explainOutput) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	pf(tw, "Selector:\t%s\n", output.Selector)
	pf(tw, "Description:\t%s\n", output.Description)
	if output.Type != nil {
		pf(tw, "Type:\t%s\n", renderJSONValue(output.Type))
	}
	if output.AllowedValues != nil {
		pf(tw, "Allowed values:\t%s\n", renderJSONValue(output.AllowedValues))
	}
	if output.Default != nil {
		pf(tw, "Default:\t%s\n", renderJSONValue(*output.Default))
	}
	if output.Required != nil {
		pf(tw, "Required:\t%t\n", *output.Required)
	}
	if output.SinceVersion != "" {
		pf(tw, "Since version:\t%s\n", output.SinceVersion)
	}
	pf(tw, "Stability:\t%s\n", output.Stability)
	pf(tw, "Example:\t%s\n", renderJSONValue(output.Example))
	pf(tw, "Build:\t%s (%s)\n", output.Version, output.Commit)
	pf(tw, "DSL version:\t%s\n", output.DSLVersion)
	_ = tw.Flush()
}

func renderJSONValue(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return strings.TrimSpace(string(raw))
}
