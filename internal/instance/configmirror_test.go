package instance

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigMirrorPathIsOptInAndAbsolute(t *testing.T) {
	for _, path := range []string{"", t.TempDir(), "relative/mirror"} {
		config := Config{ConfigMirrorPath: path}
		err := config.validateBaseConfig()
		if path == "relative/mirror" {
			if err == nil || !strings.Contains(err.Error(), "configMirrorPath must be an absolute path") {
				t.Fatalf("path=%q error=%v", path, err)
			}
		} else if err != nil {
			t.Fatalf("path=%q error=%v", path, err)
		}
	}
	data, err := json.Marshal(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "configMirrorPath") {
		t.Fatalf("unset mirror changed serialized config: %s", data)
	}
}
