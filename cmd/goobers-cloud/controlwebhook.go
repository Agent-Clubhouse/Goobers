package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/webhookintake"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/webhook"
)

// webhookPin supplies the immutable workflow and goober digests a webhook
// start is admitted against. It stays nil until the start-input resolver
// composition lands; intake then answers 503 so GitHub redelivers, and nothing
// is recorded.
var webhookPin webhookintake.Pin

// mountWebhook serves the authenticated GitHub webhook intake beside the API.
// It is mounted only when --github-webhook-secret-file is set, and the
// delivery path bypasses bearer authentication because the HMAC is its
// authentication. Intake writes through the API pool (goobers_api) with no
// controller credentials or lease.
func mountWebhook(ctx context.Context, o controlOptions, db controlDatabases, configuration control.Configuration, api http.Handler, logger *log.Logger) (http.Handler, func(), error) {
	if o.webhookSecretFile == "" {
		return api, func() {}, nil
	}
	secret, err := os.ReadFile(o.webhookSecretFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read webhook secret: %w", err)
	}
	secret = []byte(strings.TrimSpace(string(secret)))
	subs, ok := configuration.(webhookintake.Subscriptions)
	if !ok {
		return nil, nil, errors.New("webhook intake requires a configuration that resolves webhook recipients")
	}
	store, err := acceptance.New(db.API.Pools.Transaction, db.InstanceID)
	if err != nil {
		return nil, nil, err
	}
	hook, stop, err := webhookintake.NewHandler(ctx, secret, &webhookintake.Intake{Store: store, Subs: subs, Pin: webhookPin}, logger)
	if err != nil {
		return nil, nil, err
	}
	return httpapi.WrapWithRoute(api, webhook.Path, hook), stop, nil
}
