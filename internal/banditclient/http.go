// Package banditclient accesses the closed observation plane without database
// credentials. The engine worker mints a short-lived bearer for each run.
package banditclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/daemonclient"
	"github.com/goobers/goobers/internal/planehttp"
)

// TokenMinter binds each request to its run, as on the live journal plane.
type TokenMinter interface {
	Mint(runID string, ttl time.Duration) (string, error)
}

// HTTP is the remote observation seam. A missing bearer fails closed.
type HTTP struct {
	BaseURL string
	Token   string
	Minter  TokenMinter
	Client  *http.Client
}

// Observe retries only replay-safe operations: a read or an immutable outcome
// whose exact identity and content the store verifies on every retry.
func (h *HTTP) Observe(ctx context.Context, input bandit.ObservationRequest) (bandit.ObservationResponse, error) {
	if err := input.Validate(); err != nil {
		return bandit.ObservationResponse{}, err
	}
	token := h.Token
	if token == "" && h.Minter != nil {
		var err error
		token, err = h.Minter.Mint(input.RunID, 5*time.Minute)
		if err != nil {
			return bandit.ObservationResponse{}, err
		}
	}
	client := h.Client
	if client == nil {
		client = daemonclient.NewHTTP(30 * time.Second)
	}
	plane, err := planehttp.New(planehttp.Config{BaseURL: h.BaseURL, Token: token, Client: client})
	if err != nil {
		return bandit.ObservationResponse{}, err
	}
	response, err := plane.DoJSONRetrying(ctx, http.MethodPost, apicontract.BanditObservationsPath, input, nil, true)
	if err != nil {
		return bandit.ObservationResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := planehttp.ReadBounded(response.Body, 64<<20)
	if err != nil {
		return bandit.ObservationResponse{}, err
	}
	if response.StatusCode == http.StatusConflict {
		return bandit.ObservationResponse{}, bandit.ErrObservationConflict
	}
	if response.StatusCode != http.StatusOK {
		return bandit.ObservationResponse{}, fmt.Errorf("bandit plane refused request (HTTP %d)", response.StatusCode)
	}
	var output bandit.ObservationResponse
	if err := json.Unmarshal(body, &output); err != nil {
		return bandit.ObservationResponse{}, err
	}
	for _, observation := range output.Observations {
		if observation.Stage != input.Stage || observation.Schema != bandit.Schema {
			return bandit.ObservationResponse{}, errors.New("bandit plane returned an invalid sample domain")
		}
		if _, err := json.Marshal(observation); err != nil {
			return bandit.ObservationResponse{}, err
		}
	}
	return output, nil
}

var _ bandit.ObservationPlane = (*HTTP)(nil)
