package dslmigrate

import (
	"errors"
	"strings"
	"testing"
)

func TestV21PreviewAnnotationSourceStyles(t *testing.T) {
	entry := `goobers.dev/allow-preview-features: "true"`
	for _, tc := range []struct{ name, before, after string }{
		{"missing annotations", "metadata:\n  name: demo\n", "metadata:\n  name: demo\n  annotations: {" + entry + "}\n"},
		{"flow metadata", "metadata: {name: demo}\n", "metadata: {annotations: {" + entry + "}, name: demo}\n"},
		{"block annotations", "metadata:\n  name: demo\n  annotations:\n    team: '保持' # keep\n", "metadata:\n  name: demo\n  annotations:\n    team: '保持' # keep\n    " + entry + "\n"},
		{"flow annotations", "metadata: {annotations: {team: '保持'}, name: demo}\n", "metadata: {annotations: {" + entry + ", team: '保持'}, name: demo}\n"},
		{"empty annotations", "metadata: {name: demo, annotations: {}}\n", "metadata: {name: demo, annotations: {" + entry + "}}\n"},
		{"null annotations", "metadata:\n  name: demo\n  annotations: null # keep\n", "metadata:\n  name: demo\n  annotations: {" + entry + "} # keep\n"},
		{"omitted annotations value", "metadata:\n  name: demo\n  annotations: # keep\n", "metadata:\n  name: demo\n  annotations: {" + entry + "} # keep\n"},
		{"flow null", "metadata: {name: demo, annotations: null}\n", "metadata: {name: demo, annotations: {" + entry + "}}\n"},
		{"empty metadata", "metadata: {}\n", "metadata: {annotations: {" + entry + "}}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, eol := range []string{"\n", "\r\n"} {
				before := strings.ReplaceAll("# header\ndslVersion: '2.0' # version\n"+tc.before+"spec: {tasks: []} # unchanged\n", "\n", eol)
				want := strings.ReplaceAll("# header\ndslVersion: '2.1' # version\n"+tc.after+"spec: {tasks: []} # unchanged\n", "\n", eol)
				result, err := Migrate([]byte(before), "2.1")
				if err != nil {
					t.Fatal(err)
				}
				if result.After != want {
					t.Fatalf("unexpected byte edits\ngot: %q\nwant: %q", result.After, want)
				}
				if len(result.PolicyAdditions) != 0 {
					t.Fatal("preview opt-in was conflated with external policy authority")
				}
				if _, err := Migrate([]byte(result.After), "2.1"); !errors.Is(err, ErrAlreadyAtTarget) {
					t.Fatalf("not idempotent: %v", err)
				}
			}
		})
	}
}

func TestV21ExistingPreviewTruePreservedByteForByte(t *testing.T) {
	for _, metadata := range []string{
		"metadata:\n  annotations:\n    goobers.dev/allow-preview-features: 'true' # author choice\n    custom: preserved\n",
		`metadata: {annotations: {goobers.dev/allow-preview-features: "true", custom: preserved}}` + "\n",
		"metadata:\n  annotations:\n    goobers.dev/allow-preview-features: true\n",
	} {
		before := "dslVersion: '2.0'\n" + metadata + "spec: {tasks: []}\n"
		result, err := Migrate([]byte(before), "2.1")
		if err != nil {
			t.Fatal(err)
		}
		if result.After != strings.Replace(before, "'2.0'", "'2.1'", 1) {
			t.Fatal("rewrote existing preview annotation")
		}
	}
}

func TestV21RefusesOtherAuthoredPreviewValues(t *testing.T) {
	for _, value := range []string{`"false"`, "false", `"TRUE"`, `"yes"`, `""`, "null", "[]", "{}"} {
		source := "dslVersion: '2.0'\nmetadata: {annotations: {goobers.dev/allow-preview-features: " + value + "}}\nspec: {tasks: []}\n"
		if _, err := Migrate([]byte(source), "2.1"); err == nil || !strings.Contains(err.Error(), "explicitly does not enable preview") {
			t.Fatalf("value %s: %v", value, err)
		}
	}
}

func TestV21PreviewOptInAllFlowDocument(t *testing.T) {
	before := `{dslVersion: "2.0", metadata: {name: demo}, spec: {tasks: []}}` + "\n"
	want := `{dslVersion: "2.1", metadata: {annotations: {goobers.dev/allow-preview-features: "true"}, name: demo}, spec: {tasks: []}}` + "\n"
	result, err := Migrate([]byte(before), "2.1")
	if err != nil {
		t.Fatal(err)
	}
	if result.After != want {
		t.Fatalf("got %q, want %q", result.After, want)
	}
}
