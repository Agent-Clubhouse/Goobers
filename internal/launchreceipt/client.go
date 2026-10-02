package launchreceipt

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// Minter issues authority over exactly one immutable receipt digest.
type Minter interface {
	MintLaunchGrant(Grant, time.Duration) (string, error)
}

// Recorder must acknowledge durable persistence before its caller launches.
type Recorder interface {
	Record(context.Context, Receipt) error
}

// Client never sends a launch grant into a pod or model environment. It is
// consumed synchronously by the daemon before the dispatcher calls CreatePod.
type Client struct {
	BaseURL string
	Minter  Minter
}

// Record consumes a fresh grant, refusing ambiguous or failed acknowledgments.
func (c Client) Record(ctx context.Context, r Receipt) error {
	raw, err := r.Encode()
	if err != nil || c.Minter == nil || c.BaseURL == "" {
		return ErrInvalid
	}
	token, err := c.Minter.MintLaunchGrant(Grant{AttemptID: r.Binding.AttemptID, Digest: Digest(raw)}, MaxTTL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+apicontract.LaunchReceiptPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	// No redirect may forward the one-shot authority to another destination.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("launch receipt transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("launch receipt persistence refused")
	}
	return nil
}
