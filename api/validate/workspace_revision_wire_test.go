package validate_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func TestWorkspaceRevisionRawContractAcrossBoundaries(t *testing.T) {
	identity := `{"provider":"github","owner":"org","name":"repo"}`
	valid := `{"repository":` + identity + `,"commitSha":"` + strings.Repeat("a", 40) + `"}`
	cases := map[string]string{
		"null": `null`, "scalar": `"value"`, "array": `[]`,
		"missing-repository":    `{"commitSha":"` + strings.Repeat("a", 40) + `"}`,
		"missing-sha":           `{"repository":` + identity + `}`,
		"null-repository":       strings.Replace(valid, identity, "null", 1),
		"null-sha":              strings.Replace(valid, `"`+strings.Repeat("a", 40)+`"`, "null", 1),
		"array-repository":      strings.Replace(valid, identity, "[]", 1),
		"duplicate-identity":    strings.Replace(valid, `"name":"repo"`, `"name":"repo","name":"repo"`, 1),
		"miscased-identity":     strings.Replace(valid, `"provider"`, `"Provider"`, 1),
		"unknown-base-identity": strings.TrimSuffix(valid, "}") + `,"baseRepository":{"provider":"github","owner":"org","name":"repo","branch":"main"}}`,
		"unknown":               strings.TrimSuffix(valid, "}") + `,"unknown":true}`,
		"miscased":              strings.Replace(valid, `"commitSha"`, `"CommitSha"`, 1),
		"duplicate":             strings.TrimSuffix(valid, "}") + `,"commitSha":"` + strings.Repeat("a", 40) + `"}`,
	}
	for _, field := range []string{"sourceRef", "sourceId", "baseSha", "baseRepository"} {
		cases[field+"-null"] = strings.TrimSuffix(valid, "}") + `,"` + field + `":null}`
		cases[field+"-wrong-type"] = strings.TrimSuffix(valid, "}") + `,"` + field + `":42}`
	}
	for _, field := range []string{"url", "project", "id"} {
		for _, base := range []bool{false, true} {
			bad := strings.TrimSuffix(identity, "}") + `,"` + field + `":null}`
			key := "repository"
			if base {
				key = "baseRepository"
			}
			control := strings.Replace(valid, identity, bad, 1)
			if base {
				control = strings.TrimSuffix(valid, "}") + `,"baseRepository":` + bad + `}`
			}
			cases[key+"-"+field+"-null"] = control
		}
	}
	for name, control := range cases {
		t.Run(name, func(t *testing.T) {
			for boundary, decode := range revisionWireBoundaries(t) {
				err := decode([]byte(`{"status":"success","workspaceRevision":` + control + `}`))
				if coded := workspacerevision.FromError(err); coded == nil || coded.Code != workspacerevision.CodeInvalid {
					t.Errorf("%s lost semantic classification for %s: %v", boundary, control, err)
				}
			}
		})
	}
	for name, data := range map[string]string{
		"miscased-field":  `{"status":"success","WorkspaceRevision":` + valid + `}`,
		"duplicate-field": `{"status":"success","workspaceRevision":` + valid + `,"workspaceRevision":` + valid + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			for boundary, decode := range revisionWireBoundaries(t) {
				if err := decode([]byte(data)); err == nil {
					t.Errorf("%s accepted %s", boundary, data)
				}
			}
		})
	}
	for name, data := range map[string]string{
		"absent": `{"status":"success","extension":true}`,
		"valid":  `{"status":"success","extension":true,"workspaceRevision":` + valid + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			for boundary, decode := range revisionWireBoundaries(t) {
				if err := decode([]byte(data)); err != nil {
					t.Errorf("%s rejected legacy/valid input: %v", boundary, err)
				}
			}
		})
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ValidateJSON("workspace-revision.schema.json", []byte(valid)); err != nil {
		t.Fatal(err)
	}
}

func revisionWireBoundaries(t *testing.T) map[string]func([]byte) error {
	t.Helper()
	return map[string]func([]byte) error{
		"result": func(data []byte) error {
			var value apiv1.ResultEnvelope
			return json.Unmarshal(data, &value)
		},
		"invocation": func(data []byte) error {
			var value apiv1.InvocationEnvelope
			return json.Unmarshal(data, &value)
		},
		"journal": func(data []byte) error {
			var value journal.Event
			return json.Unmarshal(data, &value)
		},
		"result-file": func(data []byte) error {
			var value apiv1.ResultEnvelope
			return executor.MergeResultFileOutputs(&value, data)
		},
		"surrender": func(data []byte) error {
			plane, err := dispatcher.NewSurrenderDir(t.TempDir())
			if err != nil {
				return err
			}
			if err := plane.Put(context.Background(), "wire", "stage", 1, append(append([]byte(`{"result":`), data...), '}')); err != nil {
				return err
			}
			_, err = dispatcher.ReadSurrenderedResult(context.Background(), plane, "wire", "stage", 1)
			return err
		},
	}
}
