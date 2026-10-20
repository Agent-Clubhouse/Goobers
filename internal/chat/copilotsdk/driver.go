// Package copilotsdk implements persistent chat using the user's Copilot CLI.
package copilotsdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	sdk "github.com/github/copilot-sdk/go"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/harness"
)

type nativeSession interface {
	Send(context.Context, sdk.MessageOptions) (string, error)
	Abort(context.Context) error
	On(sdk.SessionEventHandler) func()
	SetModel(context.Context, string, *sdk.SetModelOptions) error
	Disconnect() error
}
type backend struct {
	create  func(context.Context, *sdk.SessionConfig) (nativeSession, string, error)
	resume  func(context.Context, string, *sdk.ResumeSessionConfig) (nativeSession, error)
	close   func() error
	home    string
	tools   []string
	servers map[string]sdk.MCPServerConfig
	policy  sdk.PermissionHandlerFunc
}

// Driver probes a transport and opens sessions within host authority.
type Driver struct {
	Adapter *harness.CopilotAdapter
	Request harness.RunRequest
	open    func(context.Context) (*backend, error)
}

// ID returns the transport identity.
func (d *Driver) ID() string { return "copilot-sdk" }
func capabilities() chat.Capabilities {
	c := chat.MinimalCapabilities()
	c.Steer = true
	c.Queue = "native"
	c.Interrupt = true
	c.Resume = "native"
	c.PlanApproval = true
	c.Questions = true
	c.Usage = "tokens"
	c.ModelSwitch = "live"
	c.LiveLevels = []chat.Level{chat.Plan, chat.Ask, chat.AutoEdit, chat.Auto, chat.FullAccess}
	c.SystemPrompt = true
	return c
}
func (d *Driver) connect(ctx context.Context) (*backend, error) {
	if d.open != nil {
		return d.open(ctx)
	}
	if d.Adapter == nil {
		return nil, fmt.Errorf("copilot-sdk: adapter required")
	}
	p, err := d.Adapter.OpenInteractive(ctx, d.Request)
	if err != nil {
		return nil, err
	}
	return &backend{home: p.Home, tools: p.AvailableTools, servers: p.Servers, policy: p.Permissions, close: p.Close, create: func(ctx context.Context, c *sdk.SessionConfig) (nativeSession, string, error) {
		s, e := p.Client.CreateSession(ctx, c)
		if e != nil {
			return nil, "", e
		}
		return s, s.SessionID, nil
	}, resume: func(ctx context.Context, id string, c *sdk.ResumeSessionConfig) (nativeSession, error) {
		s, err := p.Client.ResumeSessionWithOptions(ctx, id, c)
		if err != nil {
			return nil, err
		}
		return s, nil
	}}, nil
}

// Probe checks transport availability without sending a model prompt.
func (d *Driver) Probe(ctx context.Context) (chat.Probe, error) {
	b, err := d.connect(ctx)
	p := chat.Probe{Capabilities: capabilities()}
	if err != nil {
		p.Reason = chat.HandshakeFailed
		p.ErrorCode = "connect_failed"
		return p, err
	}
	err = b.close()
	p.HandshakeOK = err == nil
	return p, err
}

var nativeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,160}$`)

func resumeExists(home, id string) (bool, error) {
	if !nativeID.MatchString(id) {
		return false, fmt.Errorf("copilot-sdk: invalid resume identity")
	}
	if home == "" {
		return false, nil
	}
	root, err := os.OpenRoot(home)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(filepath.Join("session-state", id))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// Start opens a session within the supplied host authority.
func (d *Driver) Start(ctx context.Context, o chat.StartOptions) (chat.Session, error) {
	runCtx, cancel := context.WithCancel(ctx)
	s := newSession(runCtx, cancel, o)
	b, err := d.connect(runCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	s.backend = b
	fail := func(err error) (chat.Session, error) { return nil, errors.Join(err, s.Close()) }
	config := &sdk.SessionConfig{Model: o.Model, WorkingDirectory: o.Workspace, Streaming: new(true), AvailableTools: b.tools, MCPServers: b.servers, DisabledMCPServers: []string{"github-mcp-server"}, OnPermissionRequest: s.permission, OnUserInputRequest: s.question, OnExitPlanModeRequest: s.plan}
	if o.SystemPrompt != "" {
		config.SystemMessage = &sdk.SystemMessageConfig{Mode: "append", Content: o.SystemPrompt}
	}
	if o.ResumeNativeID != "" {
		exists, err := resumeExists(b.home, o.ResumeNativeID)
		if err != nil {
			return fail(err)
		}
		if !exists {
			s.fallback = chat.ResumeNotFound
		} else {
			timeout := o.ResumeTimeout
			if timeout <= 0 {
				timeout = 15 * time.Second
			}
			resumeCtx, stop := context.WithTimeout(runCtx, timeout)
			s.native, err = b.resume(resumeCtx, o.ResumeNativeID, &sdk.ResumeSessionConfig{Model: o.Model, WorkingDirectory: o.Workspace, Streaming: new(true), AvailableTools: b.tools, MCPServers: b.servers, DisabledMCPServers: []string{"github-mcp-server"}, SystemMessage: config.SystemMessage, OnPermissionRequest: s.permission, OnUserInputRequest: s.question, OnExitPlanModeRequest: s.plan})
			stalled := errors.Is(resumeCtx.Err(), context.DeadlineExceeded)
			stop()
			if err == nil {
				s.id = o.ResumeNativeID
			} else if stalled && runCtx.Err() == nil {
				// A stalled RPC may still create native writers: stop and join the old
				// server before trying a fresh session on a new connection.
				if err := b.close(); err != nil {
					return fail(err)
				}
				b, err = d.connect(runCtx)
				if err != nil {
					return fail(err)
				}
				s.backend = b
				config.AvailableTools = b.tools
				config.MCPServers = b.servers
				s.fallback = chat.ResumeStalled
			} else {
				return fail(fmt.Errorf("copilot-sdk: resume failed"))
			}
		}
	}
	if s.native == nil {
		startup, stop := context.WithTimeout(runCtx, 30*time.Second)
		s.native, s.id, err = b.create(startup, config)
		stop()
		if err != nil {
			return fail(fmt.Errorf("copilot-sdk: create session failed"))
		}
	}
	s.emit("session_started", map[string]any{"driver": d.ID(), "nativeSessionId": s.id, "capabilities": capabilities(), "resumeFallback": s.fallback}, "")
	s.unsubscribe = s.native.On(s.onEvent)
	go func() { <-runCtx.Done(); _ = s.Close() }()
	return s, nil
}
