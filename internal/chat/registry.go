package chat

import (
	"context"
	"errors"
	"fmt"
	"github.com/goobers/goobers/internal/invoke"
	"time"
)

// Registry is immutable after host setup. Registration order is fallback order
// within a provider; there is never cross-provider fallback.
type Registry struct{ drivers map[string][]Driver }

func NewRegistry() *Registry { return &Registry{drivers: map[string][]Driver{}} }
func (r *Registry) Register(provider string, d Driver) error {
	if provider == "" || d == nil {
		return fmt.Errorf("chat: invalid registration")
	}
	for _, old := range r.drivers[provider] {
		if old.ID() == d.ID() {
			return fmt.Errorf("chat: duplicate driver")
		}
	}
	r.drivers[provider] = append(r.drivers[provider], d)
	return nil
}
func (r *Registry) Start(ctx context.Context, provider string, o StartOptions) (Session, []Attempt, error) {
	var attempts []Attempt
	for _, d := range r.drivers[provider] {
		if err := ctx.Err(); err != nil {
			return nil, attempts, err
		}
		a := Attempt{Driver: d.ID()}
		begin := time.Now()
		p, err := d.Probe(ctx)
		a.CheckMS = time.Since(begin).Milliseconds()
		if err != nil || !p.HandshakeOK {
			a.Reason = p.Reason
			if a.Reason == "" {
				a.Reason = HandshakeFailed
			}
			a.ErrorCode = p.ErrorCode
			attempts = append(attempts, a)
			if errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
				return nil, attempts, err
			}
			continue
		}
		begin = time.Now()
		s, err := d.Start(ctx, o)
		a.SessionStartMS = time.Since(begin).Milliseconds()
		if err == nil && s != nil {
			return s, append(attempts, a), nil
		}
		a.Reason = StartFailed
		attempts = append(attempts, a)
		// A partially started session must be joined before trying another writer.
		if s != nil {
			err = errors.Join(err, s.Close())
		}
		if errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
			return nil, attempts, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, attempts, err
	}
	return nil, attempts, fmt.Errorf("chat: no available driver for %q", provider)
}
