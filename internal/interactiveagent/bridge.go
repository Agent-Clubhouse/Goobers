package interactiveagent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Bridge exposes a private invocation-bound tool endpoint. Its socket location
// is a local transport endpoint, not a credential. Its
// parent directory and token live exactly as long as the host invocation.
// Connections must authenticate before host callbacks supply scoped authority.
type Bridge struct {
	SocketPath string
	// TokenPath is passed only to this invocation's MCP stdio process.
	TokenPath       string
	token           []byte
	ctx             context.Context
	cancel          context.CancelFunc
	listener        net.Listener
	directory       string
	options         Options
	tools           map[string]compiledTool
	definitions     []Tool
	invokeSlot      chan struct{}
	connectionSlots chan struct{}
	mu              sync.Mutex
	connections     map[net.Conn]struct{}
	closed          bool
	once            sync.Once
	closeErr        error
	callbacks       sync.WaitGroup
}

// Start starts a private tool bridge for one bound invocation.
func Start(ctx context.Context, options Options) (*Bridge, error) {
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
	default:
		return nil, ErrUnavailable
	}
	if options.Check == nil || options.Invoke == nil {
		return nil, ErrInvalidConfiguration
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = 2 * time.Second
	}
	if options.ShutdownTimeout > 30*time.Second {
		return nil, ErrInvalidConfiguration
	}
	tools, definitions, err := compileTools(options.Tools)
	if err != nil {
		return nil, err
	}
	// Runtime parents must be short: Darwin limits Unix socket path length.
	parent := options.RuntimeDir
	if parent == "" {
		parent = "/tmp"
	}
	directory, err := os.MkdirTemp(parent, "gbr-agent-")
	if err != nil {
		return nil, ErrUnavailable
	}
	if os.Chmod(directory, 0700) != nil {
		_ = os.RemoveAll(directory)
		return nil, ErrUnavailable
	}
	socket := filepath.Join(directory, "rpc.sock")
	tokenPath := filepath.Join(directory, "auth.token")
	token, err := createToken(tokenPath)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, ErrUnavailable
	}
	if os.Chmod(socket, 0600) != nil {
		_ = listener.Close()
		_ = os.RemoveAll(directory)
		return nil, ErrUnavailable
	}
	bound, cancel := context.WithCancel(ctx)
	b := &Bridge{SocketPath: socket, TokenPath: tokenPath, token: token, ctx: bound, cancel: cancel, listener: listener, directory: directory, options: options, tools: tools, definitions: definitions, invokeSlot: make(chan struct{}, 1), connectionSlots: make(chan struct{}, 8), connections: map[net.Conn]struct{}{}}
	go b.accept()
	go func() { <-bound.Done(); _ = b.Close() }()
	return b, nil
}

// ToolNames returns the invocation’s allowed tool names.
func (b *Bridge) ToolNames() []string {
	names := make([]string, 0, len(b.definitions))
	for _, tool := range b.definitions {
		names = append(names, tool.Name)
	}
	return names
}

func (b *Bridge) accept() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		select {
		case b.connectionSlots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			_ = conn.Close()
			<-b.connectionSlots
			return
		}
		b.connections[conn] = struct{}{}
		b.mu.Unlock()
		go func() {
			defer func() { _ = conn.Close(); b.mu.Lock(); delete(b.connections, conn); b.mu.Unlock(); <-b.connectionSlots }()
			b.serveConnection(conn)
		}()
	}
}

// Close cancels queued/in-flight callbacks, closes sockets, and removes the
// private socket directory. Invoke must honor its context to stop host work.
func (b *Bridge) Close() error {
	b.once.Do(func() {
		b.cancel()
		b.mu.Lock()
		b.closed = true
		_ = b.listener.Close()
		for conn := range b.connections {
			_ = conn.Close()
		}
		b.mu.Unlock()
		if os.RemoveAll(b.directory) != nil {
			b.closeErr = ErrUnavailable
		}
		drained := make(chan struct{})
		go func() { b.callbacks.Wait(); close(drained) }()
		timer := time.NewTimer(b.options.ShutdownTimeout)
		defer timer.Stop()
		select {
		case <-drained:
		case <-timer.C:
			b.closeErr = errors.Join(b.closeErr, ErrNotQuiescent)
		}
	})
	return b.closeErr
}

func (b *Bridge) beginCallback() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.ctx.Err() != nil {
		return false
	}
	b.callbacks.Add(1)
	return true
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 4096)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > MaxRequestBytes {
			return nil, ErrInvalidMessage
		}
		line = append(line, fragment...)
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return line, nil
		}
		return nil, err
	}
}

// ServeStdio authenticates using tokenPath, then proxies bounded MCP input to one
// invocation's Unix socket and forwards responses to stdout. It writes no logs.
// On cancellation/early socket failure it closes in when in is an io.Closer.
// A custom non-closable input must itself be finite or cancellation-aware.
func ServeStdio(ctx context.Context, socketPath, tokenPath string, in io.Reader, out io.Writer) error {
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	conn, err := connectAuthenticatedBridge(ctx, socketPath, tokenPath)
	if err != nil {
		return err
	}
	// Best-effort cleanup; the operation result is reported separately.
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	inputDone := make(chan error, 1)
	outputDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReaderSize(in, 4096)
		for {
			line, err := readLine(reader)
			if err != nil {
				if errors.Is(err, io.EOF) {
					if unix, ok := conn.(*net.UnixConn); ok {
						_ = unix.CloseWrite()
					}
					inputDone <- nil
				} else {
					inputDone <- ErrInvalidMessage
				}
				return
			}
			if len(line) == 0 {
				continue
			}
			if line[len(line)-1] != '\n' {
				line = append(line, '\n')
			}
			if _, err = conn.Write(line); err != nil {
				inputDone <- ErrUnavailable
				return
			}
		}
	}()
	go func() {
		_, err := io.Copy(out, conn)
		if err != nil {
			outputDone <- ErrUnavailable
		} else {
			outputDone <- nil
		}
	}()
	closeInput := func() {
		if closer, ok := in.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	for {
		select {
		case <-ctx.Done():
			closeInput()
			return ErrUnavailable
		case err := <-inputDone:
			inputDone = nil
			if err != nil {
				closeInput()
				return err
			}
		case err := <-outputDone:
			closeInput()
			return err
		}
	}
}

func connectAuthenticatedBridge(ctx context.Context, socketPath, tokenPath string) (net.Conn, error) {
	token, err := readToken(tokenPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return nil, ErrUnavailable
	}
	dir, err := os.Lstat(filepath.Dir(socketPath))
	if err != nil || !dir.IsDir() || dir.Mode().Perm() != 0700 {
		return nil, ErrUnavailable
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, ErrUnavailable
	}
	if err := authenticateClient(conn, token); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
