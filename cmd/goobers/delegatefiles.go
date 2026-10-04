package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
)

type delegateFileProtocol struct {
	pendingDir                  string
	requestSuffix               string
	responseSuffix              string
	errorPrefix                 string
	requestDirLabel             string
	requestLabel                string
	staleAfter                  time.Duration
	distinguishNonDirectoryPath bool
}

func writeDelegateRequest[T any](
	schedulerDir string,
	cfg delegateFileProtocol,
	req T,
	mutate func(*T),
) (string, error) {
	return writeDelegateRequestWithID(schedulerDir, cfg, "", req, mutate, nil)
}

func writeDelegateRequestWithID[T any](
	schedulerDir string,
	cfg delegateFileProtocol,
	requestID string,
	req T,
	mutate func(*T),
	beforeCreate func(string) error,
) (string, error) {
	reqDir := filepath.Join(schedulerDir, cfg.pendingDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return "", fmt.Errorf("%s: create %s: %w", cfg.errorPrefix, cfg.requestDirectoryLabel(), err)
	}
	if beforeCreate != nil {
		if err := beforeCreate(reqDir); err != nil {
			return "", err
		}
	}
	f, err := os.CreateTemp(reqDir, ".pending-*")
	if err != nil {
		return "", fmt.Errorf("%s: create %s: %w", cfg.errorPrefix, cfg.requestFileLabel(), err)
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	if mutate != nil {
		mutate(&req)
	}
	data, err := json.Marshal(req)
	if err != nil {
		cleanup()
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return "", fmt.Errorf("%s: write %s: %w", cfg.errorPrefix, cfg.requestFileLabel(), err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("%s: close %s: %w", cfg.errorPrefix, cfg.requestFileLabel(), err)
	}
	if requestID == "" {
		requestID = strings.TrimPrefix(filepath.Base(tmpPath), ".pending-")
	}
	finalPath := filepath.Join(reqDir, requestID+cfg.requestSuffix)
	if err := durability.ReplaceFile(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("%s: publish %s: %w", cfg.errorPrefix, cfg.requestFileLabel(), err)
	}
	return requestID, nil
}

func (cfg delegateFileProtocol) requestDirectoryLabel() string {
	if cfg.requestDirLabel != "" {
		return cfg.requestDirLabel
	}
	return "request dir"
}

func (cfg delegateFileProtocol) requestFileLabel() string {
	if cfg.requestLabel != "" {
		return cfg.requestLabel
	}
	return "request"
}

func readAndRemoveDelegateJSON[T any](path string) (T, bool) {
	var value T
	data, err := os.ReadFile(path)
	if err != nil {
		return value, false
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, false
	}
	_ = os.Remove(path)
	return value, true
}

type delegateJSONEncodeError struct {
	err error
}

func (e *delegateJSONEncodeError) Error() string {
	return e.err.Error()
}

func (e *delegateJSONEncodeError) Unwrap() error {
	return e.err
}

func writeDelegateJSON[T any](path string, value T) error {
	data, err := json.Marshal(value)
	if err != nil {
		return &delegateJSONEncodeError{err: err}
	}
	return journal.WriteFileAtomic(path, data, 0o644)
}

func removeExpiredDelegateArtifact(path string, info os.FileInfo, now time.Time, staleAfter time.Duration) {
	if now.Sub(info.ModTime()) > staleAfter {
		_ = os.Remove(path)
	}
}

func pollDelegateResponse[T any](
	ctx context.Context,
	schedulerDir, requestID string,
	cfg delegateFileProtocol,
	timeout time.Duration,
	timeoutMessage func(string) string,
) (T, error) {
	respPath := filepath.Join(schedulerDir, cfg.pendingDir, requestID+cfg.responseSuffix)
	deadline := time.Now().Add(timeout)
	for {
		if resp, ok := readAndRemoveDelegateJSON[T](respPath); ok {
			return resp, nil
		}
		if time.Now().After(deadline) {
			var zero T
			requestPath := filepath.Join(schedulerDir, cfg.pendingDir, requestID+cfg.requestSuffix)
			return zero, errors.New(timeoutMessage(requestPath))
		}
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(delegationPollInterval):
		}
	}
}

func sweepDelegateRequests[Req, Resp any](
	schedulerDir string,
	cfg delegateFileProtocol,
	now func() time.Time,
	validate func(string, Req, error) (Resp, bool),
	handle func(Req) Resp,
) error {
	reqDir := filepath.Join(schedulerDir, cfg.pendingDir)
	var entries []os.DirEntry
	var err error
	if cfg.distinguishNonDirectoryPath {
		var exists bool
		entries, exists, err = readDirectory(reqDir)
		if !exists {
			return nil
		}
	} else {
		entries, err = os.ReadDir(reqDir)
		if os.IsNotExist(err) {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("%s: read pending requests: %w", cfg.errorPrefix, err)
	}

	var sweepErr error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(reqDir, entry.Name())
		if strings.HasSuffix(entry.Name(), cfg.responseSuffix) {
			info, err := entry.Info()
			if err == nil {
				removeExpiredDelegateArtifact(path, info, now(), cfg.staleAfter)
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), cfg.requestSuffix) {
			continue
		}
		requestID := strings.TrimSuffix(entry.Name(), cfg.requestSuffix)
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("%s: read request %s: %w", cfg.errorPrefix, requestID, err))
			}
			continue
		}
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("%s: consume request %s: %w", cfg.errorPrefix, requestID, err))
			}
			continue
		}

		var req Req
		decodeErr := json.Unmarshal(data, &req)
		resp, dispatch := validate(requestID, req, decodeErr)
		if dispatch {
			resp = handle(req)
		}
		if err := writeDelegateJSON(filepath.Join(reqDir, requestID+cfg.responseSuffix), resp); err != nil {
			var encodeErr *delegateJSONEncodeError
			if errors.As(err, &encodeErr) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("%s: encode response %s: %w", cfg.errorPrefix, requestID, err))
				continue
			}
			sweepErr = errors.Join(sweepErr, fmt.Errorf("%s: write response %s: %w", cfg.errorPrefix, requestID, err))
		}
	}
	return sweepErr
}
