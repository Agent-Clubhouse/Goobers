package validate

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWellFormedForeignGaggleValidatesClean is MGV-4's positive case (#1011): a
// fully, correctly configured foreign gaggle — its own repo/backlog credential
// connections, a read-only reference repo, a non-Go CI command, and a distinct
// branch namespace — must validate with zero errors. It is the counterpart to
// the per-failure-mode diagnostics (connectionRef / ciCommand / branchNamespace)
// and guards against a diagnostic growing overzealous and flagging a valid
// foreign-gaggle shape.
func TestWellFormedForeignGaggleValidatesClean(t *testing.T) {
	config := `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: acme-instance
spec:
  instance:
    name: acme
    environment: dev
  connections:
    - name: github-web
      type: repo
      provider: github
      secretRef:
        name: github-pat
    - name: github-backlog
      type: backlog
      provider: github
      secretRef:
        name: github-pat
    - name: github-docs
      type: repo
      provider: github
      secretRef:
        name: github-docs-pat
  gaggles:
    - acme-web
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: acme-web
spec:
  displayName: Acme Web
  project:
    provider: github
    owner: acme
    name: web
    connectionRef: github-web
  additionalRepos:
    - provider: github
      owner: acme
      name: docs
      connectionRef: github-docs
  backlog:
    provider: github
    project: acme/web
    connectionRef: github-backlog
  ciCommand: ["npm", "run", "ci"]
  branchNamespace: acme-web/
  isolation:
    namespace: gaggle-acme-web
    identityRef: acme-web-identity
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "instance.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("a well-formed foreign gaggle reported errors:\n%s", joinIssues(report))
	}
}
