package schemas_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/gagglebundle"
	"github.com/goobers/goobers/internal/instance"
)

func TestGaggleBundleSchemaAcceptsExport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	bundle, err := gagglebundle.Export(instance.NewLayout(root).ConfigDir(), "example", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	for _, file := range schemas.Files() {
		raw, err := schemas.FS.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if err := compiler.AddResource(schemas.BaseURI+file, bytes.NewReader(raw)); err != nil {
			t.Fatalf("add %s: %v", file, err)
		}
	}
	schema, err := compiler.Compile(schemas.BaseURI + schemas.GaggleBundle)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(document); err != nil {
		t.Fatalf("exported bundle does not satisfy public schema: %v", err)
	}
}
