package providers

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestBasicAuth(t *testing.T) {
	const want = "Basic YnVpbGQtYWdlbnQ6YWRvLXBhdC0wMTIzNDU2Nzg5"
	if got := basicAuth("build-agent", "ado-pat-0123456789"); got != want {
		t.Fatalf("basicAuth() = %q, want %q", got, want)
	}
}

func TestADOProviderRegistersBasicAuthCredential(t *testing.T) {
	const token = "ado-pat-0123456789"
	reg := journal.NewRegistryScrubber()
	NewADOProvider("org", "project", token,
		func(p *ADOProvider) { p.Username = "build-agent" },
		WithADOSecretRegistrar(reg),
	)

	encoded := strings.TrimPrefix(basicAuth("build-agent", token), "Basic ")
	for _, credential := range []string{token, encoded} {
		got := reg.Scrub([]byte("captured credential: " + credential))
		if bytes.Contains(got, []byte(credential)) || !bytes.Contains(got, []byte(journal.Redacted)) {
			t.Fatalf("registered credential was not redacted: %q", got)
		}
	}
}
