package stageenv

import (
	"os"
	"testing"
)

func TestNilLookupReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv("GOOBERS_STAGEENV_TEST", "from-process")
	var env Lookup
	if got := env.Get("GOOBERS_STAGEENV_TEST"); got != "from-process" {
		t.Fatalf("nil Lookup Get = %q, want the process value", got)
	}
	if err := os.Unsetenv("GOOBERS_STAGEENV_TEST"); err != nil {
		t.Fatal(err)
	}
	if got := env.Get("GOOBERS_STAGEENV_TEST"); got != "" {
		t.Fatalf("nil Lookup Get of an unset variable = %q, want empty", got)
	}
}

func TestLookupReadsOnlyItsOwnVariables(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"GOOBERS_CRED_REPO_PUSH": "per-test-token"}
	env := Lookup(func(key string) (string, bool) {
		value, ok := vars[key]
		return value, ok
	})
	if got := env.Get("GOOBERS_CRED_REPO_PUSH"); got != "per-test-token" {
		t.Fatalf("Get = %q, want the supplied value", got)
	}
	if got := env.Get("PATH"); got != "" {
		t.Fatalf("Get(PATH) = %q, want empty: a supplied Lookup must not fall through to the process", got)
	}
}
