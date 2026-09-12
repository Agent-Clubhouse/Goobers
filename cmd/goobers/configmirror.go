package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/configmirror"
)

// publishConfigMirror runs under the reload mutex. Comparing the extracted
// snapshot with appliedDigest prevents a concurrently installed, rejected Git
// tree from becoming the worker configuration.
func (r *configReloader) publishConfigMirror(ctx context.Context) error {
	if r.setup == nil || r.setup.Config == nil || r.setup.Config.ConfigMirrorPath == "" {
		return nil
	}
	if r.appliedDigest == "" {
		return errors.New("config mirror has no applied configuration digest")
	}
	if r.mirroredDigest == r.appliedDigest {
		return nil
	}
	config := *r.setup.Config
	destination := config.ConfigMirrorPath
	// Workers consume the rendered tree. They must neither fetch its source
	// repository nor publish a mirror of their own.
	config.WorkflowSource = nil
	config.ConfigMirrorPath = ""
	document, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	err = configmirror.PublishValidated(ctx, destination, r.layout.ConfigDir(), document, func(staged string) error {
		digest, err := configDirectoryDigest(filepath.Join(staged, "config"))
		if err != nil {
			return err
		}
		if digest != r.appliedDigest {
			return errors.New("captured config mirror does not match applied configuration")
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("publish rendered config mirror: %w", err)
	}
	r.mirroredDigest = r.appliedDigest
	return nil
}

func (r *configReloader) refreshConfigMirror(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := r.publishConfigMirror(ctx)
	if err == nil {
		r.lastMirrorError = ""
		return
	}
	if message := err.Error(); message != r.lastMirrorError {
		log.Printf("config mirror unavailable (will retry): %s", message)
		r.lastMirrorError = message
	}
}

// Mirror retries are independent of config watching: a temporary share outage
// must recover even when there is no subsequent edit or Git revision change.
func (r *configReloader) startConfigMirror(ctx context.Context) func() {
	if r.setup.Config.ConfigMirrorPath == "" {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			r.mu.Lock()
			r.refreshConfigMirror(ctx)
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
