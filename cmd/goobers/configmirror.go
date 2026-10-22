package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/configmirror"
	"github.com/goobers/goobers/internal/httpapi"
)

type daemonWorkerConfigService struct {
	snapshots   *configmirror.Plane
	generations *blobstore.Dir
}

func (s daemonWorkerConfigService) Current() string { return s.snapshots.Current() }

func (s daemonWorkerConfigService) Open(digest string) (io.ReadCloser, error) {
	file, err := s.snapshots.Open(digest)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return file, err
	}
	data, err := s.generations.GetBounded(context.Background(), digest, configgeneration.MaxArchiveBytes)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (u *upSession) configureWorkerConfigPlane() error {
	u.configDigests = newConfigDigestPublisher(u.setup.ConfigDigest)
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithConfigDigest(u.configDigests.Get))
	if strings.TrimSpace(u.setup.Config.API.PodTokenKeyFile) == "" {
		return nil
	}
	plane, err := configmirror.NewPlane(filepath.Join(u.l.SchedulerDir(), "worker-config-plane"))
	if err != nil {
		return err
	}
	reloader := &configReloader{
		layout:          u.l,
		setup:           u.setup,
		appliedDigest:   u.setup.ConfigDigest,
		configSnapshots: plane,
	}
	if err := reloader.publishConfigMirror(u.ctx); err != nil {
		return err
	}
	u.configSnapshots = plane
	generations, err := blobstore.NewDir(filepath.Join(u.l.BlobStoreDir(), "config-generations"))
	if err != nil {
		return err
	}
	service := daemonWorkerConfigService{snapshots: plane, generations: generations}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithConfigSnapshotService(service))
	return nil
}

// publishConfigMirror runs under the reload mutex. Comparing the extracted
// snapshot with appliedDigest prevents a concurrently installed, rejected Git
// tree from becoming the worker configuration.
func (r *configReloader) publishConfigMirror(ctx context.Context) error {
	if r.setup == nil || r.setup.Config == nil {
		return nil
	}
	destination := r.setup.Config.ConfigMirrorPath
	if destination == "" && r.configSnapshots == nil {
		return nil
	}
	if r.appliedDigest == "" {
		return errors.New("config mirror has no applied configuration digest")
	}
	if r.mirroredDigest == r.appliedDigest && r.planeTreeDigest == r.appliedDigest {
		return nil
	}
	config := *r.setup.Config
	// Workers consume the rendered tree. They must neither fetch its source
	// repository nor publish a mirror of their own.
	config.WorkflowSource = nil
	config.ConfigMirrorPath = ""
	document, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	validate := func(staged string) error {
		digest, err := configDirectoryDigest(filepath.Join(staged, "config"))
		if err != nil {
			return err
		}
		if digest != r.appliedDigest {
			return errors.New("captured config mirror does not match applied configuration")
		}
		return nil
	}
	if r.configSnapshots != nil && r.planeTreeDigest != r.appliedDigest {
		if _, err := r.configSnapshots.PublishValidated(ctx, r.layout.ConfigDir(), document, validate); err != nil {
			return fmt.Errorf("publish worker config plane: %w", err)
		}
		r.planeTreeDigest = r.appliedDigest
	}
	if destination != "" && r.mirroredDigest != r.appliedDigest {
		if err := configmirror.PublishValidated(ctx, destination, r.layout.ConfigDir(), document, validate); err != nil {
			return fmt.Errorf("publish rendered config mirror: %w", err)
		}
		r.mirroredDigest = r.appliedDigest
	}
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
	if r.setup.Config.ConfigMirrorPath == "" && r.configSnapshots == nil {
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
