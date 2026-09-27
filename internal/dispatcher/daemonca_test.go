package dispatcher

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/daemonclient"
)

// privateCADaemon is a TLS daemon, behind a certificate that chains to no
// system root, serving the credential, blob and surrender planes. It counts
// requests that got past the handshake and records handshake refusals.
type privateCADaemon struct {
	server     *httptest.Server
	caPEM      string
	served     atomic.Int64
	mu         sync.Mutex
	handshakes strings.Builder
}

func (d *privateCADaemon) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.handshakes.Write(p)
}

// refusedHandshake waits for the server to log a handshake refusal; the log
// lands on the server's connection goroutine, after the client has returned.
func (d *privateCADaemon) refusedHandshake() bool {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		d.mu.Lock()
		logged := strings.Contains(d.handshakes.String(), "TLS handshake error")
		d.mu.Unlock()
		if logged {
			return true
		}
	}
	return false
}

func newPrivateCADaemon(t *testing.T) *privateCADaemon {
	t.Helper()
	daemon := &privateCADaemon{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemon.served.Add(1)
		switch {
		case r.URL.Path == apicontract.CredentialResolvePath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"credentials": []map[string]string{{"capability": "contents:write", "value": "minted"}},
			})
		case strings.HasPrefix(r.URL.Path, BlobPathPrefix):
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("blob"))
			}
		case strings.HasSuffix(r.URL.Path, "/surrender"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	server.Config.ErrorLog = log.New(daemon, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	daemon.server = server
	daemon.caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	return daemon
}

// daemonCARetryDeadline bounds the retrying clients' refused case.
const daemonCARetryDeadline = 50 * time.Millisecond

// daemonAPICalls drives every dispatcher daemon-API client through its
// default (nil Client) path.
var daemonAPICalls = map[string]func(ctx context.Context, base string) error{
	"credential resolve": func(ctx context.Context, base string) error {
		client := &CredentialResolveClient{BaseURL: base, Token: "tok", RetryDeadline: daemonCARetryDeadline, RetryPolicy: fastRetryPolicy()}
		_, err := client.Resolve(ctx, "run-1", "stage", []string{"contents:write"})
		return err
	},
	"blob get": func(ctx context.Context, base string) error {
		_, err := (&BlobClient{BaseURL: base, Token: "tok"}).Get(ctx, "sha256:abc")
		return err
	},
	"blob put": func(ctx context.Context, base string) error {
		client := &BlobClient{BaseURL: base, Token: "tok", RetryDeadline: daemonCARetryDeadline, RetryPolicy: fastRetryPolicy()}
		return client.Put(ctx, "sha256:abc", []byte("blob"))
	},
	"stage credentials": func(ctx context.Context, base string) error {
		_, err := ResolveStageCredentials(ctx, nil, base, "tok", StageCredentialRequest{RunID: "run-1", Stage: "stage"})
		return err
	},
	"surrender": func(ctx context.Context, base string) error {
		client := &SurrenderPutClient{BaseURL: base, Token: "tok", RetryDeadline: daemonCARetryDeadline, RetryPolicy: fastRetryPolicy()}
		return client.Put(ctx, "run-1", "stage", 1, []byte(`{}`))
	},
}

// With the daemon CA configured every client reaches a private-CA daemon;
// unset, each is refused at the TLS handshake and never reaches a handler
// (#5286). The retrying clients report their last attempt's error, which the
// retry deadline may have cut short, so refusal is asserted on the daemon's
// side rather than on the error's type.
func TestDaemonAPIClientsTrustTheConfiguredDaemonCA(t *testing.T) {
	for name, call := range daemonAPICalls {
		t.Run(name+"/configured", func(t *testing.T) {
			daemon := newPrivateCADaemon(t)
			t.Setenv(daemonclient.CAEnv, daemon.caPEM)
			if err := call(context.Background(), daemon.server.URL); err != nil {
				t.Fatalf("configured daemon CA: %v", err)
			}
			if daemon.served.Load() == 0 {
				t.Fatal("configured daemon CA: the daemon served no request")
			}
		})
		t.Run(name+"/unset", func(t *testing.T) {
			daemon := newPrivateCADaemon(t)
			t.Setenv(daemonclient.CAEnv, "")
			if err := call(context.Background(), daemon.server.URL); err == nil {
				t.Fatal("unset daemon CA reached a private-CA daemon")
			}
			if served := daemon.served.Load(); served != 0 {
				t.Fatalf("unset daemon CA: the daemon served %d request(s), want 0", served)
			}
			if !daemon.refusedHandshake() {
				t.Fatal("unset daemon CA: the daemon logged no TLS handshake refusal")
			}
		})
	}
}
