package gitexclude

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type faultyTemp struct {
	excludeTempFile
	writeErr, syncErr, closeErr error
}

func (f faultyTemp) Write(b []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.excludeTempFile.Write(b)
}

func (f faultyTemp) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.excludeTempFile.Sync()
}

func (f faultyTemp) Close() error {
	err := f.excludeTempFile.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func TestReplaceExcludeFailuresLeaveNoTempFiles(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		wrap    func(excludeTempFile) excludeTempFile
		replace func(string, string) error
	}{
		{name: "write", wrap: func(f excludeTempFile) excludeTempFile { return faultyTemp{f, boom, nil, nil} }},
		{name: "sync", wrap: func(f excludeTempFile) excludeTempFile { return faultyTemp{f, nil, boom, nil} }},
		{name: "close", wrap: func(f excludeTempFile) excludeTempFile { return faultyTemp{f, nil, nil, boom} }},
		{name: "replace", replace: func(string, string) error { return boom }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origCreate, origReplace := createExcludeTemp, replaceExcludeFile
			t.Cleanup(func() { createExcludeTemp, replaceExcludeFile = origCreate, origReplace })
			createExcludeTemp = func(dir, p string) (excludeTempFile, error) {
				f, err := os.CreateTemp(dir, p)
				if err != nil {
					return nil, err
				}
				if tt.wrap != nil {
					return tt.wrap(f), nil
				}
				return f, nil
			}
			if tt.replace != nil {
				replaceExcludeFile = tt.replace
			}
			dir := t.TempDir()
			if err := replaceExclude(filepath.Join(dir, "exclude"), []byte("x")); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want boom", err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("leftover files: %v", entries)
			}
		})
	}
}

func TestReplaceExcludeCreateTempFailure(t *testing.T) {
	err := replaceExclude(filepath.Join(t.TempDir(), "missing", "exclude"), []byte("x"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReplaceExcludeSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exclude")
	if err := replaceExclude(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "x" {
		t.Fatalf("got %q", b)
	}
}
