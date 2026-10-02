package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/instance"
)

const temporalKeyDirectory = "/run/goobers/temporal-keys"

// A stable, separately provisioned Secret survives manifest regeneration. The
// generator never creates or replaces key material as part of a rollout.
func configureTemporalCodec(cfg *instance.Config, secret string) error {
	if secret == "" {
		return nil
	}
	if settings := cfg.TemporalPayloadCodec(); settings != nil && settings.KeyRef != nil {
		return fmt.Errorf("instance already configures a Temporal codec; set --temporal-codec-key-secret= and supply its key access explicitly")
	}
	const store = "temporal-history"
	for _, entry := range cfg.SecretStores {
		if entry.Name == store {
			return fmt.Errorf("secret store temporal-history is already declared")
		}
	}
	cfg.SecretStores = append(cfg.SecretStores, instance.SecretStoreConfig{Name: store, Kind: instance.SecretStoreKindFileKey, Directory: temporalKeyDirectory})
	cfg.Temporal = &instance.TemporalConfig{PayloadCodec: &instance.PayloadCodecConfig{KeyRef: &instance.KeyRef{Store: store, Name: "history"}, Strict: false}}
	return nil
}

// Projected Secret files are symlinks and group-readable. Copy only into a
// private memory volume as the workload UID; file-key deliberately refuses
// projected symlinks. Only daemon/worker processes receive this volume.
func mountTemporalCodecKey(p *corev1.PodSpec, secret string) {
	mode := int32(0440)
	p.Volumes = append(p.Volumes,
		corev1.Volume{Name: "temporal-key-source", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret, DefaultMode: &mode}}},
		corev1.Volume{Name: "temporal-keys", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
	)
	init := &p.InitContainers[0]
	init.VolumeMounts = append(init.VolumeMounts,
		corev1.VolumeMount{Name: "temporal-key-source", MountPath: "/temporal-key-source", ReadOnly: true},
		corev1.VolumeMount{Name: "temporal-keys", MountPath: temporalKeyDirectory},
	)
	init.Args[0] += "\numask 077\nmkdir -p /run/goobers/temporal-keys/history\nchmod 700 /run/goobers/temporal-keys /run/goobers/temporal-keys/history\ncp /temporal-key-source/active /temporal-key-source/*.pem /run/goobers/temporal-keys/history/\nchmod 600 /run/goobers/temporal-keys/history/*\n"
	p.Containers[0].VolumeMounts = append(p.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "temporal-keys", MountPath: temporalKeyDirectory, ReadOnly: true})
}
