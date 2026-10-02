package main

import (
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/instance"
)

func TestReferenceTemporalCodecKeyAndConfigReachBothClients(t *testing.T) {
	o := fixture(t)
	o.TemporalCodecKeySecret = "history-key"
	docs := generated(t, o)
	deployments := 0
	for _, doc := range docs {
		switch doc["kind"] {
		case "Secret":
			var secret corev1.Secret
			decode(t, doc, &secret)
			cfg, err := instance.LoadConfig(filepath.Join(unpackBundle(t, secret.Data), "instance.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			codec := cfg.TemporalPayloadCodec()
			if codec == nil || codec.KeyRef == nil || codec.KeyRef.Store != "temporal-history" || codec.KeyRef.Name != "history" || codec.Strict {
				t.Fatal("reference did not opt in compatibly")
			}
		case "Deployment":
			var dep appsv1.Deployment
			decode(t, doc, &dep)
			deployments++
			if volume(t, dep, "temporal-key-source").Secret.SecretName != "history-key" {
				t.Fatal("wrong key secret")
			}
			if volume(t, dep, "temporal-keys").EmptyDir.Medium != corev1.StorageMediumMemory {
				t.Fatal("key copy is not ephemeral memory")
			}
			init := dep.Spec.Template.Spec.InitContainers[0]
			if !strings.Contains(init.Args[0], "chmod 700 /run/goobers/temporal-keys") || !strings.Contains(init.Args[0], "chmod 600 /run/goobers/temporal-keys/history/*") {
				t.Fatal("copied key permissions violate file-key contract")
			}
			mounted := false
			for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
				if m.Name == "temporal-key-source" {
					t.Fatal("projected source exposed to runtime")
				}
				if m.Name == "temporal-keys" {
					mounted = m.ReadOnly && m.MountPath == temporalKeyDirectory
				}
			}
			if !mounted {
				t.Fatal("runtime has no private read-only key volume")
			}
		}
	}
	if deployments != 2 {
		t.Fatalf("key granted to unexpected deployments: %d", deployments)
	}
	o.Out = filepath.Join(t.TempDir(), "reused")
	o.TemporalCodecKeySecret = o.TokenSecret
	if err := prepare(o); err == nil {
		t.Fatal("accepted shared signing/wrapping Secret")
	}
	cfg := &instance.Config{Temporal: &instance.TemporalConfig{PayloadCodec: &instance.PayloadCodecConfig{KeyRef: &instance.KeyRef{Store: "existing", Name: "history"}}}}
	if err := configureTemporalCodec(cfg, "history-key"); err == nil {
		t.Fatal("overwrote existing key reference")
	}
}
