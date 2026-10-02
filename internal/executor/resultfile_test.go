package executor

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestMergeResultFileOutputsWorkspaceRevision(t *testing.T) {
	sha := strings.Repeat("a", 40)
	valid := `{"repository":{"provider":"github","owner":"org","name":"repo"},"commitSha":"` + sha + `"}`
	for _, tc := range []struct {
		name, control string
		wantErr       bool
	}{
		{name: "valid", control: valid},
		{name: "null", control: `null`, wantErr: true},
		{name: "string", control: `"value"`, wantErr: true},
		{name: "number", control: `42`, wantErr: true},
		{name: "boolean", control: `true`, wantErr: true},
		{name: "array", control: `[]`, wantErr: true},
		{name: "empty", control: `{}`, wantErr: true},
		{name: "sha", control: strings.Replace(valid, sha, "short", 1), wantErr: true},
		{name: "identity", control: strings.Replace(valid, `"org"`, `"../org"`, 1), wantErr: true},
		{name: "unknown-control", control: strings.TrimSuffix(valid, "}") + `,"unknown":true}`, wantErr: true},
		{name: "unknown-repository", control: strings.Replace(valid, `"name":"repo"`, `"name":"repo","unknown":true`, 1), wantErr: true},
		{name: "unknown-base", control: strings.TrimSuffix(valid, "}") + `,"baseRepository":{"provider":"github","owner":"org","name":"base","unknown":true}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := apiv1.ResultEnvelope{}
			err := MergeResultFileOutputs(&result, []byte(`{"legacy":"kept","workspaceRevision":`+tc.control+`}`))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if _, ok := result.Outputs["workspaceRevision"]; ok {
				t.Fatal("control was promoted as a scalar output")
			}
			if tc.wantErr {
				if result.WorkspaceRevision != nil {
					t.Fatalf("invalid control established authority: %+v", result.WorkspaceRevision)
				}
			} else if result.WorkspaceRevision == nil || result.WorkspaceRevision.CommitSHA != sha || result.Outputs["legacy"] != "kept" {
				t.Fatalf("revision or scalar output was lost: %+v", result)
			}
		})
	}
}

func TestMergeResultFileOutputsPreservesLegacyBehavior(t *testing.T) {
	for _, data := range []string{"", "not JSON", `{"broken":`, `null`, `42`, `[]`} {
		result := apiv1.ResultEnvelope{Outputs: map[string]interface{}{"existing": "kept"}}
		if err := MergeResultFileOutputs(&result, []byte(data)); err != nil {
			t.Fatalf("%q: %v", data, err)
		}
		if len(result.Outputs) != 1 || result.Outputs["existing"] != "kept" || result.WorkspaceRevision != nil {
			t.Fatalf("%q changed legacy result: %+v", data, result)
		}
	}
	result := apiv1.ResultEnvelope{Outputs: map[string]interface{}{"existing": "kept"}}
	if err := MergeResultFileOutputs(&result, []byte(`{"string":"text","number":3,"bool":true,"object":{},"array":[],"null":null}`)); err != nil {
		t.Fatal(err)
	}
	if len(result.Outputs) != 4 || result.Outputs["existing"] != "kept" || result.Outputs["string"] != "text" || result.Outputs["number"] != float64(3) || result.Outputs["bool"] != true {
		t.Fatalf("legacy scalars changed: %+v", result.Outputs)
	}
}
