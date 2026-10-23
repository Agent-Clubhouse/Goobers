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

func TestTemporalValuesPinValidatedFinalImages(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	const (
		serverPin = "1.31.4@sha256:1986d4554cc0da8cf09ddcaa10bc348f22f192972b4c2ec1f768f7307c5892e6"
		adminPin  = "1.31.4@sha256:c286fe515148587228d692806bc6f7652be5febc384a73dfd16379aed69f7d87"
	)
	for _, path := range []string{
		filepath.Join(root, "deploy", "reference", "temporal", "values.yaml"),
		filepath.Join(root, "infra", "temporal", "values.yaml"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, pin := range []string{serverPin, adminPin} {
			if !strings.Contains(string(data), `tag: "`+pin+`"`) {
				t.Errorf("%s is missing validated image pin %s", path, pin)
			}
		}
	}
	ladder, err := os.ReadFile(filepath.Join(root, "test", "ha", "kind", "cmd", "nightly", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ladder), serverPin) {
		t.Errorf("nightly final step does not match shipped server pin %s", serverPin)
	}
	namespaceJob, err := os.ReadFile(filepath.Join(root, "deploy", "reference", "temporal", "namespace-job.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(namespaceJob), adminPin) {
		t.Errorf("namespace registration does not match shipped admin-tools pin %s", adminPin)
	}
}
