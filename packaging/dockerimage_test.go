package packaging

import (
	"os"
	"regexp"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestProductImageBasesAreDigestPinnedAndUpdateable(t *testing.T) {
	dockerfile, err := os.ReadFile("docker/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, variable := range []string{"GO_IMAGE", "PORTAL_NODE_IMAGE", "RUNTIME_IMAGE"} {
		pattern := regexp.MustCompile(`(?m)^ARG ` + variable + `=[^\s@]+:[^\s@]+@sha256:[a-f0-9]{64}$`)
		if !pattern.Match(dockerfile) {
			t.Errorf("%s must retain a readable tag and immutable sha256 digest", variable)
		}
	}

	data, err := os.ReadFile("../.github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Updates []struct {
			Ecosystem   string   `json:"package-ecosystem"`
			Directory   string   `json:"directory"`
			Directories []string `json:"directories"`
		} `json:"updates"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"docker:/packaging/docker":                     false,
		"pip:/deploy/monitoring":                       false,
		"docker:/deploy/monitoring":                    false,
		"pip:/evals":                                   false,
		"maven:/test/e2e/testdata/javaservice":         false,
		"nuget:/test/e2e/testdata/dotnetservice/src":   false,
		"nuget:/test/e2e/testdata/dotnetservice/tests": false,
	}
	for _, update := range config.Updates {
		dirs := append([]string{update.Directory}, update.Directories...)
		for _, dir := range dirs {
			key := update.Ecosystem + ":" + dir
			if _, tracked := want[key]; tracked {
				want[key] = true
			}
		}
	}
	for update, found := range want {
		if !found {
			t.Errorf("Dependabot update source %s is missing", update)
		}
	}
}
