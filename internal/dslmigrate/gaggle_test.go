package dslmigrate

import (
	"strings"
	"testing"
)

func TestMigrateGaggleToV3(t *testing.T) {
	source := "apiVersion: goobers.dev/v1alpha1\nkind: Gaggle\nmetadata: {name: example}\nspec:\n  # Keep the platform requirement.\n  requiredCapabilities: [os=darwin, xcode]\n"
	result, err := MigrateGaggleToV3([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"os: macOS", "capabilities: [xcode]", "# Keep the platform requirement."} {
		if !strings.Contains(result.After, want) {
			t.Errorf("migration missing %q: %s", want, result.After)
		}
	}
	if strings.Contains(result.After, "dslVersion") || strings.Contains(result.After, "requiredCapabilities:") {
		t.Fatalf("unexpected legacy or version field: %s", result.After)
	}
	again, err := MigrateGaggleToV3([]byte(result.After))
	if err != nil || again.Changed || again.After != result.After {
		t.Fatalf("repeat migration = %+v, %v", again, err)
	}
}

func TestMigrateGaggleToV3RefusesUnsafeInput(t *testing.T) {
	for _, tail := range []string{
		"  requiredCapabilities: os=linux\n",
		"  requiredCapabilities: [12]\n",
		"  requiredCapabilities: [os=linux, os=windows]\n",
		"  requiredCapabilities: [go]\n  runsOn: {capabilities: [git]}\n",
		"  sandbox: enforced\n",
		"  requiredCapabilities: &caps [go]\n  another: *caps\n",
		"  requiredCapabilities: [go]\n---\nkind: Gaggle\n",
	} {
		t.Run(tail, func(t *testing.T) {
			if _, err := MigrateGaggleToV3([]byte("kind: Gaggle\nspec:\n" + tail)); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}
}
