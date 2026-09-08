package investigation

import (
	"bytes"
	"encoding/json"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/api/schemas"
)

var evidenceSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	return compileEvidenceSchema(schemas.InvestigationEvidence)
})

var draftSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	return compileEvidenceSchema(schemas.InvestigationEvidenceDraft)
})

func compileEvidenceSchema(target string) (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	for _, name := range []string{schemas.Envelope["artifact"], schemas.InvestigationEvidence, schemas.InvestigationEvidenceDraft} {
		data, err := schemas.FS.ReadFile(name)
		if err != nil {
			return nil, err
		}
		if err := compiler.AddResource("https://goobers.dev/schemas/"+name, bytes.NewReader(data)); err != nil {
			return nil, err
		}
	}
	return compiler.Compile("https://goobers.dev/schemas/" + target)
}

func validateEvidenceJSON(data []byte) error {
	schema, err := evidenceSchema()
	if err != nil {
		return err
	}
	return validateSchemaJSON(schema, data)
}

func validateSchemaJSON(schema *jsonschema.Schema, data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return schema.Validate(value)
}
