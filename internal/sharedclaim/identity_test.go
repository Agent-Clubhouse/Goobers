package sharedclaim

import (
	"context"
	"strings"
	"testing"
	"time"
)

type forbiddenIdentityStore struct{ t *testing.T }

func (s forbiddenIdentityStore) Read(context.Context, string) (Observation, error) {
	s.t.Fatal("invalid identity reached provider read")
	return Observation{}, nil
}

func (s forbiddenIdentityStore) CompareAndSwap(context.Context, string, string, Record) error {
	s.t.Fatal("invalid identity reached provider write")
	return nil
}

func TestInvalidIdentitiesFailBeforeProviderAccess(t *testing.T) {
	for _, field := range []string{"key", "instance", "run", "token"} {
		for _, value := range []string{"", "invalid\xff", "nul\x00", "line\n", "tab\t", "delete\x7f", "control\u0085", strings.Repeat("x", 1025)} {
			t.Run(field+"/"+value, func(t *testing.T) {
				key, owner := "42", (Owner{"instance", "run", "token"})
				switch field {
				case "key":
					key = value
				case "instance":
					owner.Instance = value
				case "run":
					owner.Run = value
				case "token":
					owner.Token = value
				}
				store := forbiddenIdentityStore{t}
				if err := Acquire(t.Context(), store, key, owner, time.Minute); err == nil {
					t.Fatal("invalid identity acquired")
				}
				if err := Release(t.Context(), store, key, owner); err == nil {
					t.Fatal("invalid identity released")
				}
				if _, err := Encode(key, Record{Version: 1, Owner: owner, ExpiresAt: time.Now().UTC()}); err == nil {
					t.Fatal("invalid identity encoded")
				}
			})
		}
	}
}

func TestPrintableIdentitiesRoundTripWithoutNormalization(t *testing.T) {
	for _, value := range []string{"ordinary", "café", "cafe\u0301", "replacement-\ufffd", strings.Repeat("x", 256)} {
		owner := Owner{value, value, value}
		record := Record{Version: 1, Owner: owner, ExpiresAt: time.Now().UTC()}
		data, err := Encode(value, record)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(value, data)
		if err != nil || decoded != record {
			t.Fatalf("identity changed during round trip: %+v %v", decoded, err)
		}
	}
}
