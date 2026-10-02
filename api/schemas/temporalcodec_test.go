package schemas

import "testing"

func TestInstanceSchemaTemporalPayloadCodec(t *testing.T) {
	schema := compileInstanceSchema(t)
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"{}", true}, {"{payloadCodec: {}}", true},
		{"{payloadCodec: {keyRef: {store: keys, name: history}, strict: true}}", true},
		{"{payloadCodec: {strict: true}}", false},
		{"{payloadCodec: {keyRef: 'keys/history'}}", false},
		{"{payloadCodec: {keyRef: {store: keys, name: '../history'}}}", false},
		{"{payloadCodec: {keyRef: {store: keys, name: history, version: v1}}}", true},
		{"{payloadCodec: {keyRef: {store: keys, name: history}, unexpected: true}}", false},
	} {
		if err := validateInstanceYAML(t, schema, "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\ntemporal: "+tc.value+"\n"); (err == nil) != tc.valid {
			t.Fatalf("%s valid=%v err=%v", tc.value, tc.valid, err)
		}
	}
}
