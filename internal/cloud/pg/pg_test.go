package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/jackc/pgx/v5"
)

const testURL = "postgres://user:password@localhost:6432/goobers"

func TestPoolExecModeSafeForTransactionPooling(t *testing.T) {
	transaction, direct, err := PoolConfigs(Config{
		TransactionURL: testURL,
		DirectURL:      "postgres://user:password@localhost:5432/goobers",
		MaxConnections: 9, DirectMaxConnections: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]*pgx.ConnConfig{
		"transaction": transaction.ConnConfig,
		"direct":      direct.ConnConfig,
	} {
		if config.DefaultQueryExecMode != pgx.QueryExecModeExec ||
			config.StatementCacheCapacity != 0 || config.DescriptionCacheCapacity != 0 {
			t.Fatalf("%s mode=%v statement-cache=%d description-cache=%d",
				name, config.DefaultQueryExecMode, config.StatementCacheCapacity, config.DescriptionCacheCapacity)
		}
	}
	if transaction.MaxConns != 7 || direct.MaxConns != 2 {
		t.Fatalf("connection budget transaction=%d direct=%d", transaction.MaxConns, direct.MaxConns)
	}
}

func TestEntraTokenProviderRefreshes(t *testing.T) {
	credential := &recordingCredential{}
	provider := EntraTokenProvider{Credential: credential}
	transaction, direct, err := PoolConfigs(Config{
		TransactionURL: testURL,
		DirectURL:      "postgres://user@localhost:5432/goobers",
		TokenProvider:  provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, second := transaction.ConnConfig.Copy(), direct.ConnConfig.Copy()
	if err := transaction.BeforeConnect(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := direct.BeforeConnect(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if first.Password != "token-1" || second.Password != "token-2" || credential.calls != 2 {
		t.Fatalf("passwords=%q,%q calls=%d", first.Password, second.Password, credential.calls)
	}
	if len(credential.options.Scopes) != 1 || credential.options.Scopes[0] != PostgresEntraScope {
		t.Fatalf("token options = %+v", credential.options)
	}
}

func TestPoolConfigRejectsOverBudgetDirectPool(t *testing.T) {
	_, _, err := PoolConfigs(Config{
		TransactionURL: testURL, DirectURL: testURL,
		MaxConnections: 4, DirectMaxConnections: 4,
	})
	if err == nil {
		t.Fatal("over-budget direct pool accepted")
	}
}

type recordingCredential struct {
	calls   int
	options policy.TokenRequestOptions
	err     error
}

func (c *recordingCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls++
	c.options = options
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
	return azcore.AccessToken{Token: "token-" + string(rune('0'+c.calls)), ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestEntraTokenProviderWrapsCredentialFailure(t *testing.T) {
	cause := errors.New("identity unavailable")
	config, _, err := PoolConfigs(Config{
		TransactionURL: testURL, DirectURL: testURL,
		TokenProvider: EntraTokenProvider{Credential: &recordingCredential{err: cause}},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = config.BeforeConnect(t.Context(), config.ConnConfig.Copy())
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v", err)
	}
}
