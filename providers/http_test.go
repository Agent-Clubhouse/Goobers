package providers

import "testing"

func TestBasicAuth(t *testing.T) {
	const want = "Basic YnVpbGQtYWdlbnQ6YWRvLXBhdC0wMTIzNDU2Nzg5"
	if got := basicAuth("build-agent", "ado-pat-0123456789"); got != want {
		t.Fatalf("basicAuth() = %q, want %q", got, want)
	}
}
