package temporalcodec

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/goobers/goobers/internal/oidcauth"
)

func testOIDC(t *testing.T, keys *testKeys) (oidcauth.Config, func(string) string) {
	t.Helper()
	key := keys.versions["v1"]
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/.well-known/openid-configuration" {
			_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, issuer.URL, issuer.URL+"/jwks")
			return
		}
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"test","n":%q,"e":"AQAB"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()))
	}))
	t.Cleanup(issuer.Close)
	cfg := oidcauth.Config{Issuer: issuer.URL, Audience: "codec", Roles: oidcauth.RoleMapping{View: []string{"readers"}, Operate: []string{"operators"}, Admin: []string{"admins"}}}
	sign := func(role string) string {
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer.URL, "aud": "codec", "sub": "viewer", "exp": time.Now().Add(time.Hour).Unix(), "roles": []string{role}})
		token.Header["kid"] = "test"
		encoded, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	return cfg, sign
}

func codecRequest(h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	return response
}

func TestHTTPCodecOIDCRolesAndRemoteContract(t *testing.T) {
	codec, keys := newTestCodec(t)
	auth, sign := testOIDC(t, keys)
	handler, err := NewHTTPHandler(codec, auth, []string{"https://temporal.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	plain := testPayload(t)
	sealed, err := codec.Encode([]*commonpb.Payload{plain})
	if err != nil {
		t.Fatal(err)
	}
	body, err := protojson.Marshal(&commonpb.Payloads{Payloads: sealed})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/encode", "/decode"} {
		for _, tc := range []struct {
			role   string
			status int
		}{{"", 401}, {"invalid", 401}, {"outsiders", 403}, {"readers", 200}, {"operators", 200}, {"admins", 200}} {
			t.Run(route+tc.role, func(t *testing.T) {
				token := tc.role
				if token != "" && token != "invalid" {
					token = sign(tc.role)
				}
				response := codecRequest(handler, route, token, string(body))
				if response.Code != tc.status {
					t.Fatalf("status=%d want=%d", response.Code, tc.status)
				}
				if response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("response can be cached")
				}
				if tc.status != 200 && strings.Contains(response.Body.String(), "sensitive") {
					t.Fatal("denial leaked payload content")
				}
			})
		}
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	remote := converter.NewRemotePayloadCodec(converter.RemotePayloadCodecOptions{Endpoint: server.URL, Client: *server.Client(), ModifyRequest: func(r *http.Request) error { r.Header.Set("Authorization", "Bearer "+sign("readers")); return nil }})
	encoded, err := remote.Encode([]*commonpb.Payload{plain})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := remote.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || !proto.Equal(decoded[0], plain) {
		t.Fatal("SDK remote contract roundtrip mismatch")
	}
}

func TestHTTPCodecBoundsAndSafeErrors(t *testing.T) {
	codec, keys := newTestCodec(t)
	auth, sign := testOIDC(t, keys)
	handler, err := NewHTTPHandler(codec, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := sign("readers")
	for _, body := range []string{`{"payloads":[{"data":"!!!sensitive!!!"}]}`, `{"unexpected":"sensitive"}`, strings.Repeat(" ", MaxHTTPBodyBytes+1)} {
		response := codecRequest(handler, "/decode", token, body)
		if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "sensitive") {
			t.Fatalf("unsafe error: %d %s", response.Code, response.Body.String())
		}
	}
	sealed, err := codec.Encode([]*commonpb.Payload{testPayload(t)})
	if err != nil {
		t.Fatal(err)
	}
	sealed[0].Data[0] ^= 1
	body, err := protojson.Marshal(&commonpb.Payloads{Payloads: sealed})
	if err != nil {
		t.Fatal(err)
	}
	if response := codecRequest(handler, "/decode", token, string(body)); response.Code != 400 || response.Body.String() != "codec operation failed\n" {
		t.Fatal("tampering response leaked detail")
	}
	keys.fail = true
	if response := codecRequest(handler, "/encode", token, `{"payloads":[{}]}`); response.Code != 400 || response.Body.String() != "codec operation failed\n" {
		t.Fatal("backend error leaked detail")
	}
}

func TestHTTPCodecRequiresAuthConfigAndExplicitCORS(t *testing.T) {
	codec, keys := newTestCodec(t)
	auth, sign := testOIDC(t, keys)
	if _, err := NewHTTPHandler(codec, oidcauth.Config{}, nil); err == nil {
		t.Fatal("allowed anonymous handler")
	}
	if _, err := NewHTTPHandler(nil, auth, nil); err == nil {
		t.Fatal("allowed absent codec")
	}
	for _, origin := range []string{"*", "null", "https://example.com/path", "https://user@example.com", "https://example.com?q=1"} {
		if _, err := NewHTTPHandler(codec, auth, []string{origin}); err == nil {
			t.Fatalf("accepted origin %q", origin)
		}
	}
	handler, err := NewHTTPHandler(codec, auth, []string{"https://temporal.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, origin, token string
		status                int
	}{
		{"OPTIONS", "https://temporal.example.test", "", 204},
		{"POST", "https://temporal.example.test", "", 401},
		{"POST", "https://temporal.example.test", sign("readers"), 200},
		{"POST", "https://evil.example.test", sign("readers"), 403},
		{"GET", "", "", 405},
	} {
		request := httptest.NewRequest(tc.method, "/decode", strings.NewReader(`{"payloads":[]}`))
		request.Header.Set("Origin", tc.origin)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+tc.token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("%s %s status=%d", tc.method, tc.origin, response.Code)
		}
	}
	if keys.wraps != 0 || keys.unwraps != 0 {
		t.Fatal("preflight or empty request performed key operation")
	}
}

func TestHTTPCodecDeniesBeforeReadingBody(t *testing.T) {
	codec, keys := newTestCodec(t)
	auth, _ := testOIDC(t, keys)
	handler, err := NewHTTPHandler(codec, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/decode", nil)
	request.Body = unreadableBody{}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("missing auth was not denied")
	}
}

type unreadableBody struct{}

func (unreadableBody) Read([]byte) (int, error) { panic("unauthorized request body was read") }
func (unreadableBody) Close() error             { return nil }

func TestHTTPCodecRejectsOversizedBatchBeforeKeys(t *testing.T) {
	codec, keys := newTestCodec(t)
	auth, sign := testOIDC(t, keys)
	handler, err := NewHTTPHandler(codec, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch := &commonpb.Payloads{}
	for range MaxBatchPayloads + 1 {
		batch.Payloads = append(batch.Payloads, &commonpb.Payload{})
	}
	body, err := protojson.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	response := codecRequest(handler, "/encode", sign("readers"), string(body))
	if response.Code != 400 || keys.wraps != 0 {
		t.Fatal("batch limits did not precede key operations")
	}
	// No data-dependent response should contain the original request bytes.
	result, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(result, body) {
		t.Fatal("response echoed request")
	}
}

func TestHTTPCodecCapsConcurrentOperations(t *testing.T) {
	codec, keys := newTestCodec(t)
	auth, sign := testOIDC(t, keys)
	handler, err := NewHTTPHandler(codec, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	concrete := handler.(*httpHandler)
	for range cap(concrete.slots) {
		concrete.slots <- struct{}{}
	}
	response := codecRequest(handler, "/decode", sign("readers"), `{"payloads":[]}`)
	if response.Code != 503 || keys.unwraps != 0 {
		t.Fatal("concurrency cap failed")
	}
}
