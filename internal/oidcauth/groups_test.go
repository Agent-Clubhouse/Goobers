package oidcauth

import (
	"crypto/rsa"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/goobers/goobers/internal/httpapi"
)

func TestVerifiedGroupsFromAuthenticatedJWT(t *testing.T) {
	key, _ := testKeys(t)
	auth, err := New(Config{Issuer: "https://identity.example", Audience: "api://goobers", GroupsClaim: "teams", Roles: RoleMapping{View: []string{"viewer"}}})
	if err != nil {
		t.Fatal(err)
	}
	auth.keys = map[string]*rsa.PublicKey{"test": &key.PublicKey}
	auth.keysFetchedAt = time.Now()
	for _, tc := range []struct {
		name string
		raw  any
		want []string
	}{
		{"valid", []string{"z", "a", "z"}, []string{"a", "z"}},
		{"absent", nil, nil}, {"single string", "admins", nil}, {"mixed", []any{"admins", 7}, nil}, {"oversized string", []string{strings.Repeat("a", 513)}, nil}, {"oversized list", make([]string, 129), nil}, {"padded", []string{" admins"}, nil}, {"control", []string{"admin\nteam"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{"iss": "https://identity.example", "aud": "api://goobers", "sub": "alice", "exp": time.Now().Add(time.Minute).Unix(), "roles": []string{"viewer"}, "teams": tc.raw}
			p, err := authenticate(t, auth, mintToken(t, key, jwt.SigningMethodRS256, "test", claims))
			if err != nil {
				t.Fatal(err)
			}
			if p.Subject != "alice" || !p.HasRole(httpapi.RoleView) || !reflect.DeepEqual(p.Groups, tc.want) {
				t.Fatalf("principal=%+v", p)
			}
		})
	}
	claims := jwt.MapClaims{"iss": "https://attacker.example", "aud": "api://goobers", "sub": "alice", "exp": time.Now().Add(time.Minute).Unix(), "roles": []string{"viewer"}, "teams": []string{"admins"}}
	if _, err := authenticate(t, auth, mintToken(t, key, jwt.SigningMethodRS256, "test", claims)); err == nil {
		t.Fatal("unverified issuer supplied groups")
	}
}
