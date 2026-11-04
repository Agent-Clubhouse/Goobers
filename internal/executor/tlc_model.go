package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// TLCModel is a finite integer-labelled transition graph, not executable TLA+.
// The reviewed specification observes Candidate.state. No candidate expressions,
// assumptions, module overrides, or checker output can enter generated TLA+.
type TLCModel struct {
	States  []int   `json:"states"`
	Initial []int   `json:"initial"`
	Edges   [][]int `json:"edges"`
}

func decodeTLCJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected exactly one JSON object")
	}
	return nil
}

func compileTLCModel(data []byte) ([]byte, error) {
	var model TLCModel
	if err := decodeTLCJSON(data, &model); err != nil {
		return nil, err
	}
	if err := validateTLCModel(model); err != nil {
		return nil, err
	}
	var body strings.Builder
	body.WriteString("---- MODULE Candidate ----\nEXTENDS Integers\nVARIABLE state\nInit == state \\in {")
	for i, value := range model.Initial {
		if i > 0 {
			body.WriteString(", ")
		}
		body.WriteString(strconv.Itoa(value))
	}
	body.WriteString("}\nNext ==\n")
	if len(model.Edges) == 0 {
		body.WriteString("  FALSE\n")
	}
	for _, edge := range model.Edges {
		fmt.Fprintf(&body, "  \\/ /\\ state = %d /\\ state' = %d\n", edge[0], edge[1])
	}
	body.WriteString("====\n")
	return []byte(body.String()), nil
}

func validateTLCModel(model TLCModel) error {
	if len(model.States) == 0 || len(model.States) > 1024 || len(model.Initial) == 0 || len(model.Initial) > 1024 || len(model.Edges) > 8192 {
		return errors.New("finite graph requires 1..1024 states, nonempty initial states, and at most 8192 edges")
	}
	seen := map[int]bool{}
	for _, state := range model.States {
		if state < -1000000 || state > 1000000 || seen[state] {
			return errors.New("states must be distinct integers in [-1000000,1000000]")
		}
		seen[state] = true
	}
	for _, initial := range model.Initial {
		if !seen[initial] {
			return errors.New("initial state is not declared")
		}
	}
	for _, edge := range model.Edges {
		if len(edge) != 2 || !seen[edge[0]] || !seen[edge[1]] {
			return errors.New("transition endpoint is not declared")
		}
	}
	return nil
}

var tlcIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9]{0,127}$`)

func tlcConfiguration(invariant string) ([]byte, error) {
	if !tlcIdentifierPattern.MatchString(invariant) {
		return nil, errors.New("invariant must be a TLA+ identifier")
	}
	if slices.Contains([]string{"Init", "Next", "state"}, invariant) {
		return nil, errors.New("invariant collides with graph definitions")
	}
	return []byte("INIT Init\nNEXT Next\nINVARIANT " + invariant + "\nCHECK_DEADLOCK FALSE\n"), nil
}

func tlcWordRune(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_'
}
