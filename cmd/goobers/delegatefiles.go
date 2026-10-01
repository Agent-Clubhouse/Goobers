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
	pendingDir     string
	requestSuffix  string
	responseSuffix string
	errorPrefix    string
	staleAfter     time.Duration
}

func writeDelegateRequest[T any](
	schedulerDir string,
	cfg delegateFileProtocol,
	req T,
	mutate func(*T),
) (string, error) {
	reqDir := filepath.Join(schedulerDir, cfg.pendingDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return "", fmt.Errorf("%s: create request dir: %w", cfg.errorPrefix, err)
	}
	f, err := os.CreateTemp(reqDir, ".pending-*")
	if err != nil {
		return "", fmt.Errorf("%s: create request: %w", cfg.errorPrefix, err)
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
		return "", fmt.Errorf("%s: write request: %w", cfg.errorPrefix, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("%s: close request: %w", cfg.errorPrefix, err)
	}
	requestID := strings.TrimPrefix(filepath.Base(tmpPath), ".pending-")
	finalPath := filepath.Join(reqDir, requestID+cfg.requestSuffix)
	if err := durability.ReplaceFile(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("%s: publish request: %w", cfg.errorPrefix, err)
	}
	return requestID, nil
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
		if data, err := os.ReadFile(respPath); err == nil {
			var resp T
			if err := json.Unmarshal(data, &resp); err == nil {
				_ = os.Remove(respPath)
				return resp, nil
			}
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
	entries, exists, err := readDirectory(reqDir)
	if !exists {
		return nil
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
			if err == nil && now().Sub(info.ModTime()) > cfg.staleAfter {
				_ = os.Remove(path)
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
		respData, err := json.Marshal(resp)
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("%s: encode response %s: %w", cfg.errorPrefix, requestID, err))
			continue
		}
		if err := journal.WriteFileAtomic(filepath.Join(reqDir, requestID+cfg.responseSuffix), respData, 0o644); err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("%s: write response %s: %w", cfg.errorPrefix, requestID, err))
		}
	}
	return sweepErr
}
