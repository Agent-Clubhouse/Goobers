package executor

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const tlcNativeTestSpec = "---- MODULE ReviewedCounter ----\nEXTENDS Naturals\nVARIABLE x\nInit == x = 0\nNext == x' = x\nSpec == Init /\\ [][Next]_x\nTypeOK == x \\in Nat\nSafe == x = 0\n====\n"
const tlcNativeTestConfig = "CONSTANTS\nBound = 2\nNames = {\"a\", \"b\"}\nEnabled = TRUE\nSPECIFICATION Spec\nINVARIANTS TypeOK Safe\nCHECK_DEADLOCK FALSE\n"

func TestTLCNativeRefusesCheckerJavaOverrideCollision(t *testing.T) {
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	if _, err := writer.Create("tlc2/module/ReviewedCounter.class"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := refuseTLCNativeOverride(data.Bytes(), "ReviewedCounter"); err == nil {
		t.Fatal("native source could be replaced by a checker Java module override")
	}
	if err := refuseTLCNativeOverride(data.Bytes(), "Other"); err != nil {
		t.Fatal(err)
	}
}

func TestTLCNativeRejectsUncheckedObligations(t *testing.T) {
	for _, config := range []string{
		strings.ReplaceAll(tlcNativeTestConfig, "TypeOK Safe", "TypeOK"),
		tlcNativeTestConfig + "CONSTRAINT FALSE\n",
		tlcNativeTestConfig + "ACTION_CONSTRAINT FALSE\n",
		tlcNativeTestConfig + "PROPERTY Safe\n",
		tlcNativeTestConfig + "SIMULATE\n",
		strings.ReplaceAll(tlcNativeTestConfig, "CHECK_DEADLOCK FALSE", "CHECK_DEADLOCK TRUE"),
		strings.ReplaceAll(tlcNativeTestConfig, "SPECIFICATION Spec", "INIT Init\nNEXT Next"),
		strings.ReplaceAll(tlcNativeTestConfig, "Bound = 2", "Bound <- Other"),
		strings.ReplaceAll(tlcNativeTestConfig, "Bound = 2", "Bound = Nat"),
		tlcNativeTestConfig + "SPECIFICATION Other\n",
	} {
		if err := validateTLCNativeConfig([]byte(config), "Safe"); err == nil {
			t.Fatalf("unchecked native obligations accepted: %s", config)
		}
	}
	for _, spec := range []string{
		strings.ReplaceAll(tlcNativeTestSpec, "EXTENDS Naturals", "EXTENDS Naturals, TLC"),
		strings.ReplaceAll(tlcNativeTestSpec, "EXTENDS Naturals", "EXTENDS IOUtils"),
		tlcNativeTestSpec + "ASSUME FALSE\n",
		tlcNativeTestSpec + "THEOREM Unsafe == TRUE\nPROOF OMITTED\n",
		tlcNativeTestSpec + "LEMMA Unsafe == TRUE OBVIOUS\n",
		tlcNativeTestSpec + "INSTANCE Other\n",
		tlcNativeTestSpec + "---- MODULE Other ----\n====\n",
		strings.ReplaceAll(tlcNativeTestSpec, "MODULE ReviewedCounter", "MODULE Different"),
	} {
		attempt := &tlcAttempt{manifest: TLCManifest{Mode: tlcReviewedNative, Module: "ReviewedCounter", Invariant: "Safe"},
			spec: []byte(spec), config: []byte(tlcNativeTestConfig)}
		if err := attempt.readNative(); err == nil {
			t.Fatal("unsafe or mismatched native module accepted")
		}
	}
}

func TestTLCNativePinsActualReviewedSourceAndRefusesCandidate(t *testing.T) {
	root := tlcTestDirectory(t)
	spec, cfg := []byte(tlcNativeTestSpec), []byte(tlcNativeTestConfig)
	for name, data := range map[string][]byte{"model.tla": spec, "model.cfg": cfg} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := TLCManifest{Mode: tlcReviewedNative, Module: "ReviewedCounter", Invariant: "Safe",
		Specification: "model.tla", Configuration: "model.cfg", SpecDigest: tlcDigest(spec), ConfigDigest: tlcDigest(cfg), Assumptions: []string{}}
	env := apiv1.InvocationEnvelope{Inputs: map[string]any{}}
	attempt := &tlcAttempt{manifest: manifest}
	if err := attempt.readInputs(root, env); err != nil || string(attempt.subject) != string(spec) || string(attempt.module) != string(spec) {
		t.Fatalf("native source bytes were not the actual checked subject: %v", err)
	}
	env.Inputs[InputTLCModel] = ""
	if err := attempt.readInputs(root, env); err == nil {
		t.Fatal("native mode accepted a candidate model selector")
	}
	delete(env.Inputs, InputTLCModel)
	for _, path := range []string{"model.tla", "model.cfg"} {
		original, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), append(original, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := attempt.readInputs(root, env); err == nil {
			t.Fatalf("changed native input pin accepted: %s", path)
		}
		if err := os.WriteFile(filepath.Join(root, path), original, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if string(attempt.subject) != string(spec) {
		t.Fatal("a later source change altered the native snapshot")
	}
}
