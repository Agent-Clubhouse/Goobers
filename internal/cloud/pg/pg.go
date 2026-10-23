// Package pg owns cloud PostgreSQL connectivity.
package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DefaultMaxConnections is the default total pool budget.
	DefaultMaxConnections int32 = 16
	// DefaultDirectMaxConnections reserves direct session connections.
	DefaultDirectMaxConnections int32 = 2
	// DefaultConnectTimeout bounds initial network connection establishment.
	DefaultConnectTimeout = 10 * time.Second
	// PostgresEntraScope is the Azure Database for PostgreSQL token audience.
	PostgresEntraScope = "https://ossrdbms-aad.database.windows.net/.default"
)

// Config describes the transaction-pooled and direct PostgreSQL connections.
// MaxConnections is the total budget; DirectMaxConnections is reserved for
// migrations and notifier sessions on direct port 5432.
type Config struct {
	TransactionURL       string
	DirectURL            string
	MaxConnections       int32
	DirectMaxConnections int32
	ConnectTimeout       time.Duration
	TokenProvider        TokenProvider
}

// TokenProvider returns a fresh PostgreSQL password token.
type TokenProvider interface {
	Token(context.Context) (string, error)
}

// Pools separates PgBouncer-safe request traffic from session-bound direct
// traffic such as migrations, advisory locks, and LISTEN.
type Pools struct {
	Transaction *pgxpool.Pool
	Direct      *pgxpool.Pool
}

// Close closes both pools.
func (p *Pools) Close() {
	if p == nil {
		return
	}
	if p.Transaction != nil {
		p.Transaction.Close()
	}
	if p.Direct != nil {
		p.Direct.Close()
	}
}

// NewPools constructs both pools without opening a connection eagerly.
func NewPools(ctx context.Context, config Config) (*Pools, error) {
	transaction, direct, err := PoolConfigs(config)
	if err != nil {
		return nil, err
	}
	transactionPool, err := pgxpool.NewWithConfig(ctx, transaction)
	if err != nil {
		return nil, fmt.Errorf("cloud postgres: transaction pool: %w", err)
	}
	directPool, err := pgxpool.NewWithConfig(ctx, direct)
	if err != nil {
		transactionPool.Close()
		return nil, fmt.Errorf("cloud postgres: direct pool: %w", err)
	}
	return &Pools{Transaction: transactionPool, Direct: directPool}, nil
}

// PoolConfigs validates config and produces the two concrete pool configs.
func PoolConfigs(config Config) (*pgxpool.Config, *pgxpool.Config, error) {
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return nil, nil, err
	}
	transaction, err := pgxpool.ParseConfig(config.TransactionURL)
	if err != nil {
		return nil, nil, fmt.Errorf("cloud postgres: parse transaction URL: %w", err)
	}
	direct, err := pgxpool.ParseConfig(config.DirectURL)
	if err != nil {
		return nil, nil, fmt.Errorf("cloud postgres: parse direct URL: %w", err)
	}
	transaction.MaxConns = config.MaxConnections - config.DirectMaxConnections
	direct.MaxConns = config.DirectMaxConnections
	for _, pool := range []*pgxpool.Config{transaction, direct} {
		pool.ConnConfig.ConnectTimeout = config.ConnectTimeout
		applyExecMode(pool.ConnConfig)
		if config.TokenProvider != nil {
			pool.BeforeConnect = tokenBeforeConnect(config.TokenProvider, pool.BeforeConnect)
		}
	}
	return transaction, direct, nil
}

func (config Config) withDefaults() Config {
	if config.MaxConnections == 0 {
		config.MaxConnections = DefaultMaxConnections
	}
	if config.DirectMaxConnections == 0 {
		config.DirectMaxConnections = DefaultDirectMaxConnections
	}
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = DefaultConnectTimeout
	}
	return config
}

func (config Config) validate() error {
	switch {
	case strings.TrimSpace(config.TransactionURL) == "":
		return errors.New("cloud postgres: transaction URL is required")
	case strings.TrimSpace(config.DirectURL) == "":
		return errors.New("cloud postgres: direct URL is required")
	case config.MaxConnections < 2:
		return errors.New("cloud postgres: connection budget must be at least 2")
	case config.DirectMaxConnections < 1:
		return errors.New("cloud postgres: direct connection budget must be positive")
	case config.DirectMaxConnections >= config.MaxConnections:
		return errors.New("cloud postgres: direct connections must be smaller than the total connection budget")
	case config.ConnectTimeout < 0:
		return errors.New("cloud postgres: connect timeout must not be negative")
	}
	return nil
}

func applyExecMode(config *pgx.ConnConfig) {
	config.DefaultQueryExecMode = pgx.QueryExecModeExec
	config.StatementCacheCapacity = 0
	config.DescriptionCacheCapacity = 0
}

func tokenBeforeConnect(provider TokenProvider, prior func(context.Context, *pgx.ConnConfig) error) func(context.Context, *pgx.ConnConfig) error {
	return func(ctx context.Context, config *pgx.ConnConfig) error {
		if prior != nil {
			if err := prior(ctx, config); err != nil {
				return err
			}
		}
		token, err := provider.Token(ctx)
		if err != nil {
			return fmt.Errorf("cloud postgres: acquire Entra token: %w", err)
		}
		if strings.TrimSpace(token) == "" {
			return errors.New("cloud postgres: Entra token is empty")
		}
		config.Password = token
		return nil
	}
}

// EntraTokenProvider adapts an Azure credential to PostgreSQL password tokens.
type EntraTokenProvider struct {
	Credential azcore.TokenCredential
}

// Token gets one token for the Azure Database for PostgreSQL audience.
func (p EntraTokenProvider) Token(ctx context.Context) (string, error) {
	if p.Credential == nil {
		return "", errors.New("cloud postgres: Entra credential is required")
	}
	token, err := p.Credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{PostgresEntraScope}})
	if err != nil {
		return "", err
	}
	return token.Token, nil
}
