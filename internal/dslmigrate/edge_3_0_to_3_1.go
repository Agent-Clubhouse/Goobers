package dslmigrate

import "gopkg.in/yaml.v3"

// applyV30ToV31 deliberately changes only the version pin, which Migrate
// patches in the original source. DSL 3.1 carries forward 3.0's positional
// artifact behavior; named slots and consumer bindings are author opt-ins.
// Neither expectedOutputs nor generated artifact names establish a semantic
// contract, so this edge must not infer artifactSlots or artifactInputs from
// them. expectedOutputs remains optional and advisory.
func applyV30ToV31(_ []byte, _ *yaml.Node) (bool, []string, error) {
	return false, nil, nil
}
