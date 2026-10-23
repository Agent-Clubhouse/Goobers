package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemporalBootstrapUsesChartOneDatastoreParameters(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	pipeline, err := os.ReadFile(filepath.Join(root, "infra", "pipelines", "infra-bootstrap.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(pipeline)
	for _, want := range []string{
		`--helm-set server.config.persistence.datastores.default.sql.connectAddr="$(postgresFqdn):5432"`,
		`--helm-set server.config.persistence.datastores.visibility.sql.connectAddr="$(postgresFqdn):5432"`,
		`server.config.persistence.default.sql.host`,
		`server.config.persistence.visibility.sql.host`,
		`argocd app unset temporal --parameter "$obsolete"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("infra bootstrap is missing %q", want)
		}
	}
	for _, obsolete := range []string{
		"--helm-set server.config.persistence.default.sql.host",
		"--helm-set server.config.persistence.visibility.sql.host",
	} {
		if strings.Contains(text, obsolete) {
			t.Errorf("infra bootstrap still injects removed chart key %q", obsolete)
		}
	}
}
