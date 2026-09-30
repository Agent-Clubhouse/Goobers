package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const diskFullMarker = ".goobers-telemetry-load-volume"
const diskFullLimit = 64 << 20

type diskFullFixture struct {
	path     string
	identity os.FileInfo
	bytes    int64
	restored bool
}

func validateDiskFullVolume(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("disk-full volume must be an explicit absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect disk-full volume: %w", err)
	}
	if !info.IsDir() {
		return errors.New("disk-full volume must be an existing real directory")
	}
	if err = smallTmpfs(path); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != diskFullMarker || !entries[0].Type().IsRegular() {
		return errors.New("disk-full volume must contain only the explicit regular-file fixture marker")
	}
	return nil
}

func fillDiskFullVolume(path string) (*diskFullFixture, error) {
	// Recheck the bounded tmpfs before allocating; never fill a general disk.
	if err := smallTmpfs(path); err != nil {
		return nil, err
	}
	fixture := &diskFullFixture{path: filepath.Join(path, "load-fixture-full.bin")}
	file, err := os.OpenFile(fixture.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	fixture.identity, err = file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	buffer := make([]byte, 1<<20)
	for fixture.bytes <= diskFullLimit {
		var n int
		n, err = file.Write(buffer)
		fixture.bytes += int64(n)
		if err != nil {
			break
		}
	}
	closeErr := file.Close()
	if !errors.Is(err, syscall.ENOSPC) || closeErr != nil {
		_ = fixture.restore()
		return nil, errors.Join(errors.New("fixture did not cleanly confirm ENOSPC within its bound"), err, closeErr)
	}
	return fixture, nil
}

func (f *diskFullFixture) restore() error {
	if f.restored {
		return nil
	}
	info, err := os.Lstat(f.path)
	if err != nil {
		return fmt.Errorf("inspect disk-full fixture before removal: %w", err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(f.identity, info) {
		return errors.New("refusing to remove changed disk-full fixture")
	}
	if err := os.Remove(f.path); err != nil {
		return err
	}
	f.restored = true
	return nil
}
