package schemas

import "testing"

func TestInstanceSchemaOneCollectorReference(t *testing.T) {
	schema := compileInstanceSchema(t)
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`telemetry: {enabled: false, onecollector: {profile_file: 'C:\synthetic\profile.json'}}`, true},
		{`telemetry: {onecollector: {profile_file: '/synthetic/profile.json', endpoint: 'https://example.invalid'}}`, false},
		{`telemetry: {onecollector: {}}`, false},
	} {
		err := validateInstanceYAML(t, schema, "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\n"+test.body)
		if (err == nil) != test.valid {
			t.Fatalf("valid=%t error=%v", test.valid, err)
		}
	}
}
