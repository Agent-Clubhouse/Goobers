package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/jackc/pgx/v5/pgproto3"
)

const upstreamCredentialMarker = "review-secret-delta-sentinel"

func TestDatabaseUpstreamErrorsAreSanitized(t *testing.T) {
	for _, phase := range []string{"connect", "ping"} {
		for _, side := range []string{"transaction", "direct"} {
			t.Run(phase+"/"+side, func(t *testing.T) {
				t.Setenv("GOOBERS_OBJECT_STORE_URL", "https://example.invalid")
				failing := credentialEchoPostgres(t, phase)
				transactionURL, directURL := failing, failing
				if side == "direct" {
					transactionURL = credentialEchoPostgres(t, "ok")
				}
				t.Setenv("GOOBERS_DATABASE_URL", transactionURL)
				t.Setenv("GOOBERS_DATABASE_DIRECT_URL", directURL)
				code, _, stderr := runForTest("api")
				if code != 1 {
					t.Fatalf("code = %d, stderr = %q", code, stderr)
				}
				if strings.Contains(stderr, upstreamCredentialMarker) {
					t.Fatalf("upstream database error exposed credential: %q", stderr)
				}
				if !strings.Contains(stderr, "SQLSTATE") {
					t.Fatalf("database diagnostic omitted safe SQLSTATE: %q", stderr)
				}
			})
		}
	}
}

func TestEntraUpstreamErrorsAreSanitized(t *testing.T) {
	for _, phase := range []string{"initialize", "token"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("GOOBERS_OBJECT_STORE_URL", "https://example.invalid")
			original := newDefaultAzureCredential
			t.Cleanup(func() { newDefaultAzureCredential = original })
			newDefaultAzureCredential = func() (azcore.TokenCredential, error) {
				if phase == "initialize" {
					return nil, errors.New("upstream credential setup " + upstreamCredentialMarker)
				}
				return credentialEchoToken{}, nil
			}
			t.Setenv("GOOBERS_DATABASE_URL", "postgres://review@127.0.0.1:1/review?sslmode=disable")
			t.Setenv("GOOBERS_DATABASE_DIRECT_URL", "postgres://review@127.0.0.1:1/review?sslmode=disable")
			code, _, stderr := runForTest("api", "--database-auth", "entra")
			if code != 1 {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			if strings.Contains(stderr, upstreamCredentialMarker) {
				t.Fatalf("Entra error exposed credential: %q", stderr)
			}
			if phase == "token" && !strings.Contains(stderr, "Entra token acquisition failed (status 401)") {
				t.Fatalf("token diagnostic = %q", stderr)
			}
		})
	}
}

type credentialEchoToken struct{}

func (credentialEchoToken) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, &azidentity.AuthenticationFailedError{
		RawResponse: &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "Unauthorized",
			Body: io.NopCloser(strings.NewReader(
				`{"error_description":"assertion ` + upstreamCredentialMarker + ` rejected"}`,
			)),
		},
	}
}

func credentialEchoPostgres(t *testing.T, phase string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go serveCredentialEchoPostgres(connection, phase)
		}
	}()
	return fmt.Sprintf(
		"postgres://review:%s@%s/review?sslmode=disable",
		upstreamCredentialMarker,
		listener.Addr(),
	)
}

func serveCredentialEchoPostgres(connection net.Conn, phase string) {
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	backend := pgproto3.NewBackend(connection, connection)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		return
	}
	if err := backend.SetAuthType(pgproto3.AuthTypeCleartextPassword); err != nil {
		return
	}
	backend.Send(&pgproto3.AuthenticationCleartextPassword{})
	if backend.Flush() != nil {
		return
	}
	message, err := backend.Receive()
	if err != nil {
		return
	}
	password, ok := message.(*pgproto3.PasswordMessage)
	if !ok {
		return
	}
	if phase == "connect" {
		backend.Send(&pgproto3.ErrorResponse{
			Severity: "FATAL", Code: "28000",
			Message: "upstream authentication rejected " + password.Password,
		})
		_ = backend.Flush()
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if backend.Flush() != nil {
		return
	}
	for {
		message, err := backend.Receive()
		if err != nil {
			return
		}
		switch message.(type) {
		case *pgproto3.Terminate:
			return
		case *pgproto3.Query:
			if phase == "ping" {
				backend.Send(&pgproto3.ErrorResponse{
					Severity: "ERROR", Code: "XX000",
					Message: "upstream ping rejected " + password.Password,
				})
			} else {
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			}
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if backend.Flush() != nil {
				return
			}
		default:
			return
		}
	}
}
