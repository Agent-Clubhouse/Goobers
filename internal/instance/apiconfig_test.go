package instance

import (
	"strings"
	"testing"
)

func hardenedAPIConfig(listen string) APIConfig {
	return APIConfig{
		Listen: listen,
		TLS:    &APITLSConfig{CertFile: "/etc/goobers/api.crt", KeyFile: "/etc/goobers/api.key"},
		Auth: &APIAuthConfig{OIDC: &OIDCAuthConfig{
			Issuer:   "https://issuer.example.com/tenant/v2.0",
			Audience: "api://goobers",
			Roles:    OIDCRoleMapping{View: []string{"goobers-viewers"}},
		}},
	}
}

func TestValidateAPIListenFailClosedOffLoopback(t *testing.T) {
	tests := []struct {
		name    string
		api     APIConfig
		wantErr string
	}{
		{
			name: "default loopback posture is valid unconfigured",
			api:  APIConfig{},
		},
		{
			name: "explicit loopback stays valid without tls or auth",
			api:  APIConfig{Listen: "[::1]:9090"},
		},
		{
			name:    "non-loopback without tls and auth is refused",
			api:     APIConfig{Listen: "0.0.0.0:8443"},
			wantErr: "api.listen",
		},
		{
			name: "non-loopback with tls but no auth is refused",
			api: APIConfig{
				Listen: "0.0.0.0:8443",
				TLS:    &APITLSConfig{CertFile: "cert.pem", KeyFile: "key.pem"},
			},
			wantErr: "not loopback",
		},
		{
			name: "non-loopback with auth but no tls is refused",
			api: APIConfig{
				Listen: "10.0.0.7:8443",
				Auth: &APIAuthConfig{OIDC: &OIDCAuthConfig{
					Issuer:   "https://issuer.example.com",
					Audience: "api://goobers",
					Roles:    OIDCRoleMapping{View: []string{"viewers"}},
				}},
			},
			wantErr: "not loopback",
		},
		{
			name: "non-loopback with tls and auth is valid",
			api:  hardenedAPIConfig("0.0.0.0:8443"),
		},
		{
			name:    "hostname binds count as non-loopback",
			api:     APIConfig{Listen: "daemon.internal:8443"},
			wantErr: "not loopback",
		},
		{
			name: "loopback with tls and auth opts in early",
			api:  hardenedAPIConfig("127.0.0.1:8443"),
		},
		{
			name:    "wildcard empty host is still refused",
			api:     hardenedAPIConfig(":8443"),
			wantErr: "wildcard",
		},
		{
			name:    "tls requires both certFile and keyFile",
			api:     APIConfig{TLS: &APITLSConfig{CertFile: "cert.pem"}},
			wantErr: "api.tls",
		},
		{
			name:    "auth requires the oidc block",
			api:     APIConfig{Auth: &APIAuthConfig{}},
			wantErr: "api.auth",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{API: test.api}
			err := cfg.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestOIDCAuthConfigValidate(t *testing.T) {
	valid := OIDCAuthConfig{
		Issuer:   "https://login.microsoftonline.com/9f9f9f9f-0000-0000-0000-000000000000/v2.0",
		Audience: "api://goobers",
		Roles: OIDCRoleMapping{
			View:    []string{"11111111-aaaa-bbbb-cccc-000000000001"},
			Operate: []string{"11111111-aaaa-bbbb-cccc-000000000002"},
			Admin:   []string{"11111111-aaaa-bbbb-cccc-000000000003"},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if got := valid.RolesClaimName(); got != DefaultOIDCRolesClaim {
		t.Fatalf("RolesClaimName() = %q, want %q", got, DefaultOIDCRolesClaim)
	}
	if got := (OIDCAuthConfig{RolesClaim: "groups"}).RolesClaimName(); got != "groups" {
		t.Fatalf("RolesClaimName() = %q, want groups", got)
	}

	tests := []struct {
		name    string
		mutate  func(*OIDCAuthConfig)
		wantErr string
	}{
		{
			name:    "issuer required",
			mutate:  func(c *OIDCAuthConfig) { c.Issuer = "" },
			wantErr: "issuer is required",
		},
		{
			name:    "issuer must be absolute",
			mutate:  func(c *OIDCAuthConfig) { c.Issuer = "issuer.example.com" },
			wantErr: "absolute http(s) URL",
		},
		{
			name:    "http issuer only for loopback",
			mutate:  func(c *OIDCAuthConfig) { c.Issuer = "http://issuer.example.com" },
			wantErr: "must use https",
		},
		{
			name:   "http loopback issuer allowed for development",
			mutate: func(c *OIDCAuthConfig) { c.Issuer = "http://127.0.0.1:8123" },
		},
		{
			name:    "issuer must not carry a query",
			mutate:  func(c *OIDCAuthConfig) { c.Issuer = "https://issuer.example.com?tenant=x" },
			wantErr: "query or fragment",
		},
		{
			name:    "audience required",
			mutate:  func(c *OIDCAuthConfig) { c.Audience = "" },
			wantErr: "audience is required",
		},
		{
			name: "empty mapping denies everyone and is rejected",
			mutate: func(c *OIDCAuthConfig) {
				c.Roles = OIDCRoleMapping{}
			},
			wantErr: "at least one claim value",
		},
		{
			name: "duplicate claim value across roles is rejected",
			mutate: func(c *OIDCAuthConfig) {
				c.Roles.Operate = append(c.Roles.Operate, c.Roles.View[0])
			},
			wantErr: "mapped to both",
		},
		{
			name: "empty claim value is rejected",
			mutate: func(c *OIDCAuthConfig) {
				c.Roles.Admin = append(c.Roles.Admin, "")
			},
			wantErr: "empty claim values",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			cfg.Roles = OIDCRoleMapping{
				View:    append([]string(nil), valid.Roles.View...),
				Operate: append([]string(nil), valid.Roles.Operate...),
				Admin:   append([]string(nil), valid.Roles.Admin...),
			}
			test.mutate(&cfg)
			err := cfg.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadConfigAPIHardening(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
api:
  listen: "0.0.0.0:8443"
  tls:
    certFile: /etc/goobers/api.crt
    keyFile: /etc/goobers/api.key
  auth:
    oidc:
      issuer: https://login.microsoftonline.com/9f9f9f9f-0000-0000-0000-000000000000/v2.0
      audience: api://goobers
      rolesClaim: groups
      roles:
        view: ["11111111-aaaa-bbbb-cccc-000000000001"]
        operate: ["11111111-aaaa-bbbb-cccc-000000000002"]
        admin: ["11111111-aaaa-bbbb-cccc-000000000003"]
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.API.TLS == nil || cfg.API.TLS.CertFile != "/etc/goobers/api.crt" || cfg.API.TLS.KeyFile != "/etc/goobers/api.key" {
		t.Fatalf("unexpected TLS config: %+v", cfg.API.TLS)
	}
	oidc := cfg.API.Auth.OIDC
	if oidc == nil || oidc.Audience != "api://goobers" || oidc.RolesClaimName() != "groups" {
		t.Fatalf("unexpected OIDC config: %+v", oidc)
	}
	if len(oidc.Roles.View) != 1 || len(oidc.Roles.Operate) != 1 || len(oidc.Roles.Admin) != 1 {
		t.Fatalf("unexpected role mapping: %+v", oidc.Roles)
	}
}

// TestDefaultAPIPostureUnconfigured pins the zero-config posture #640 must
// not disturb: loopback default address, no TLS, no authenticator.
func TestDefaultAPIPostureUnconfigured(t *testing.T) {
	path := writeInstanceYAML(t, `
apiVersion: goobers.dev/v1alpha1
kind: Instance
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.APIListenAddress(); got != DefaultAPIListenAddress {
		t.Fatalf("APIListenAddress = %q, want %q", got, DefaultAPIListenAddress)
	}
	if cfg.API.TLS != nil || cfg.API.Auth != nil {
		t.Fatalf("unconfigured API gained hardening blocks: %+v", cfg.API)
	}
}
