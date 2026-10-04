package k8spreflight

import (
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporaldial"
)

func TestResolveTemporalOptions(t *testing.T) {
	root := t.TempDir()
	cfg := &instance.Config{
		Engine:       &instance.EngineConfig{HostPort: "instance:7233", Namespace: "instance-ns", TLS: &temporaldial.TLS{ServerName: "frontend.example"}},
		SecretStores: []instance.SecretStoreConfig{{Name: "keys", Kind: instance.SecretStoreKindFileKey, Directory: filepath.Join(root, "absent-keys")}},
		Temporal:     &instance.TemporalConfig{PayloadCodec: &instance.PayloadCodecConfig{KeyRef: &instance.KeyRef{Store: "keys", Name: "history"}, Strict: true}},
	}
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, root, host, namespace, wantHost, wantNamespace string
	}{
		{"rootless optional", "", "", "", "", ""},
		{"rootless explicit", "", "explicit:7233", "explicit-ns", "explicit:7233", "explicit-ns"},
		{"instance defaults", root, "", "", "instance:7233", "instance-ns"},
		{"host override", root, "explicit:7233", "", "explicit:7233", "instance-ns"},
		{"namespace override", root, "", "explicit-ns", "instance:7233", "explicit-ns"},
		{"both overrides", root, "explicit:7233", "explicit-ns", "explicit:7233", "explicit-ns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := ResolveTemporalOptions(tc.root, Options{TemporalHostPort: tc.host, TemporalNamespace: tc.namespace, PSAServiceAccount: "stage-account"})
			if err != nil {
				t.Fatal(err)
			}
			if opts.TemporalHostPort != tc.wantHost || opts.TemporalNamespace != tc.wantNamespace || opts.PSAServiceAccount != "stage-account" {
				t.Fatalf("targets or unrelated options changed: %+v", opts)
			}
			if tc.root == "" {
				if opts.TemporalTLS != nil || opts.TemporalDataConverter != converter.GetDefaultDataConverter() {
					t.Fatal("rootless options changed transport or converter defaults")
				}
				return
			}
			if opts.TemporalTLS == nil || opts.TemporalTLS.ServerName != "frontend.example" {
				t.Fatal("instance TLS settings lost")
			}
			legacy, err := converter.GetDefaultDataConverter().ToPayload("legacy")
			if err != nil {
				t.Fatal(err)
			}
			var decoded string
			if err := opts.TemporalDataConverter.FromPayload(legacy, &decoded); err == nil {
				t.Fatal("instance strict codec was not applied")
			}
		})
	}
	if _, err := ResolveTemporalOptions(t.TempDir(), Options{}); err == nil {
		t.Fatal("missing instance config accepted")
	}
	cfg.Temporal.PayloadCodec.KeyRef = nil
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTemporalOptions(root, Options{}); err == nil {
		t.Fatal("strict codec without a key accepted")
	}
}
