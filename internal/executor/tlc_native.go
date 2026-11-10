package executor

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const tlcReviewedNative = "reviewed-native"

var (
	tlcNativeHeader = regexp.MustCompile(`\A-+[ \t]+MODULE[ \t]+([A-Za-z_][A-Za-z_0-9]*)[ \t]+-+[ \t]*\r?\nEXTENDS[ \t]+([A-Za-z_, \t]+)\r?\n`)
	tlcNativeScalar = regexp.MustCompile(`^(TRUE|FALSE|[0-9]{1,9}|"[A-Za-z0-9_-]{0,128}")$`)
)

func (a *tlcAttempt) validateMode(env apiv1.InvocationEnvelope) error {
	switch a.manifest.Mode {
	case "", "finite-graph":
		if a.manifest.Module != "" || stringInput(env, InputTLCModel) == "" {
			return errors.New("finite-graph mode requires tlcModel and no native module")
		}
	case tlcReviewedNative:
		if _, present := env.Inputs[InputTLCModel]; present {
			return errors.New("reviewed-native mode refuses candidate tlcModel input")
		}
		if !tlcIdentifierPattern.MatchString(a.manifest.Module) {
			return errors.New("reviewed-native mode requires a module identifier")
		}
	default:
		return errors.New("unsupported TLC manifest mode")
	}
	return nil
}

func (a *tlcAttempt) readNative() error {
	header := tlcNativeHeader.FindSubmatch(a.spec)
	if len(header) != 3 || string(header[1]) != a.manifest.Module {
		return errors.New("reviewed native module header does not match the manifest")
	}
	for _, module := range strings.Split(string(header[2]), ",") {
		if !slices.Contains([]string{"Naturals", "Integers"}, strings.TrimSpace(module)) {
			return errors.New("reviewed native imports are restricted to Naturals and Integers")
		}
	}
	if err := validateTLCModuleWords(a.spec); err != nil {
		return err
	}
	if err := validateTLCNativeConfig(a.config, a.manifest.Invariant); err != nil {
		return err
	}
	a.subject = append([]byte(nil), a.spec...)
	a.module = append([]byte(nil), a.spec...)
	return nil
}

func validateTLCModuleWords(data []byte) error {
	modules, extensions := 0, 0
	for _, word := range strings.FieldsFunc(string(data), func(r rune) bool { return !tlcWordRune(r) }) {
		switch word {
		case "MODULE":
			modules++
		case "EXTENDS":
			extensions++
		case "ASSUME", "ASSUMPTION", "AXIOM", "THEOREM", "PROOF", "INSTANCE",
			"LEMMA", "COROLLARY", "PROPOSITION", "OMITTED", "OBVIOUS":
			return errors.New("specification contains unsupported assumptions, proof obligations, or module substitution")
		}
	}
	if modules != 1 || extensions != 1 {
		return errors.New("additional module declarations/imports are not supported")
	}
	return nil
}

func validateTLCNativeConfig(data []byte, invariant string) error {
	seen := map[string]bool{}
	constants := false
	requested := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		key := fields[0]
		switch key {
		case "CONSTANTS":
			if seen[key] || len(fields) != 1 {
				return errors.New("native constants require one CONSTANTS block")
			}
			constants = true
		case "SPECIFICATION":
			if seen[key] || len(fields) != 2 || !tlcIdentifierPattern.MatchString(fields[1]) {
				return errors.New("native configuration requires exactly one specification operator")
			}
			constants = false
		case "INVARIANT", "INVARIANTS":
			if seen["invariants"] || len(fields) < 2 {
				return errors.New("native configuration requires one explicit invariant list")
			}
			for _, name := range fields[1:] {
				if !tlcIdentifierPattern.MatchString(name) {
					return errors.New("native invariant list must contain identifiers only")
				}
				requested = requested || name == invariant
			}
			seen["invariants"] = true
			constants = false
		case "CHECK_DEADLOCK":
			if seen[key] || line != "CHECK_DEADLOCK FALSE" {
				return errors.New("native configuration must explicitly disable deadlock checking")
			}
			constants = false
		default:
			if !constants || !tlcNativeConstant(line) {
				return fmt.Errorf("unsupported native configuration directive or constant: %s", key)
			}
			continue
		}
		seen[key] = true
	}
	if !seen["SPECIFICATION"] || !seen["CHECK_DEADLOCK"] || !requested {
		return errors.New("native configuration must select the requested invariant and complete specification")
	}
	return nil
}

func tlcNativeConstant(line string) bool {
	name, value, found := strings.Cut(line, "=")
	if !found || !tlcIdentifierPattern.MatchString(strings.TrimSpace(name)) {
		return false
	}
	value = strings.TrimSpace(value)
	if tlcNativeScalar.MatchString(value) {
		return true
	}
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return false
	}
	members := strings.TrimSpace(value[1 : len(value)-1])
	if members == "" {
		return true
	}
	for _, member := range strings.Split(members, ",") {
		if !tlcNativeScalar.MatchString(strings.TrimSpace(member)) {
			return false
		}
	}
	return true
}

func refuseTLCNativeOverride(jar []byte, module string) error {
	reader, err := zip.NewReader(bytes.NewReader(jar), int64(len(jar)))
	if err != nil {
		return err
	}
	for _, entry := range reader.File {
		if entry.Name == "tlc2/module/"+module+".class" {
			return errors.New("reviewed native module collides with a checker Java override")
		}
	}
	return nil
}

func (a *tlcAttempt) sourceNames() (string, string, string) {
	if a.manifest.Mode == tlcReviewedNative {
		return a.manifest.Module + ".tla", a.manifest.Module + ".cfg", a.manifest.Module + ".tla"
	}
	return "Spec.tla", "Spec.cfg", "Candidate.tla"
}
