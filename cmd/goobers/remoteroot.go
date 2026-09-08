package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

// prepareRemoteRoot reads the target daemon, never the client's filesystem.
// Older daemons without durable identity must be upgraded before manual writes.
// Redirects are refused: a different server cannot vouch for this target.
func prepareRemoteRoot(ctx context.Context, endpoint string, diagnostic io.Writer) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+apicontract.InstancePath, nil)
	if err != nil {
		return fmt.Errorf("build daemon identity request")
	}
	request.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(os.Getenv("GOOBERS_API_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: remoteTriggerTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		// Transport errors may contain credential-bearing URLs. Do not echo them.
		return fmt.Errorf("call daemon API for root identity failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon root identity unavailable (HTTP %d); no mutation sent", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRemoteTriggerResponseBody+1))
	if err != nil || len(body) > maxRemoteTriggerResponseBody {
		return fmt.Errorf("daemon root identity response unreadable or oversized")
	}
	root, identity, err := decodeRemoteRoot(body)
	if err != nil {
		return err
	}
	if err := validateRemoteRoot(root, identity); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(diagnostic, "Remote instance root: %q; instance ID: %q\n", root, identity.ID); err != nil {
		return fmt.Errorf("display remote mutation target: %w", err)
	}
	return nil
}

func validateRemoteRoot(root string, identity *readservice.RootIdentity) error {
	if strings.TrimSpace(root) == "" || len(root) > 4096 || identity == nil {
		return fmt.Errorf("daemon has no usable durable root identity; upgrade or repair it before mutation")
	}
	id, err := hex.DecodeString(identity.ID)
	if err != nil || len(id) != 16 || strings.ToLower(identity.ID) != identity.ID || identity.IdentityProblem != "" {
		return fmt.Errorf("daemon durable root identity is unavailable or invalid")
	}
	if identity.DecommissionedAt != nil || identity.DecommissionReason != "" || identity.LifecycleProblem != "" {
		return fmt.Errorf("daemon root is historical or its lifecycle is unverifiable; no mutation sent")
	}
	return nil
}
