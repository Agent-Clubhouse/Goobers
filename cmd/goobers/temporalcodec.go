package main

import (
	"encoding/json"
	"flag"
	"io"
	"net"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/temporalcodec"
)

const temporalHelp = "Usage: goobers temporal codec-server [flags] [path]\n\nServe Temporal Web UI payload decoding over TLS with the instance OIDC view role.\n"
const temporalCodecHelp = "Usage: goobers temporal codec-server --tls-cert <pem> --tls-key <pem> [--listen 127.0.0.1:8444] [--allow-origin https://temporal.example.com] [path]\n\nRequires temporal.payloadCodec.keyRef and api.auth.oidc. Every encode/decode POST requires an OIDC bearer token with view permission. Repeat --allow-origin for each exact Web UI origin. No anonymous mode.\n"

func runTemporal(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		pf(stdout, "%s", temporalHelp)
		return 0
	}
	pf(stderr, "%s", temporalHelp)
	return 2
}

func runTemporalCodecServer(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("temporal codec-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:8444", "TLS listener address")
	cert := fs.String("tls-cert", "", "TLS certificate PEM file")
	key := fs.String("tls-key", "", "TLS private key PEM file")
	var origins repeatableFlag
	fs.Var(&origins, "allow-origin", "exact Temporal Web UI origin (repeatable)")
	fs.Usage = helpUsage(stderr, "temporal codec-server")
	if !parseFlagsBeforePath(fs, args, stderr) || fs.NArg() > 1 {
		return 2
	}
	root := fs.Arg(0)
	if root == "" {
		root = "."
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	server, err := temporalcodec.NewServer(cfg, *listen, *cert, *key, origins)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", server.Addr)
	if err != nil {
		pf(stderr, "error: listen: %v\n", err)
		return 1
	}
	pf(stdout, "Temporal codec server listening on https://%s (OIDC view required)\n", listener.Addr())
	if err := temporalcodec.Serve(ctx, server, listener); err != nil {
		pf(stderr, "error: codec server: %v\n", err)
		return 1
	}
	return 0
}

// Doctor reports configured state without fetching keys or claiming existing
// plaintext histories have been migrated.
func runDoctorTemporalCodec(root, format string, stdout, stderr io.Writer) int {
	if root == "" {
		root = "."
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	status := struct {
		Enabled      bool             `json:"enabled"`
		Strict       bool             `json:"strict"`
		KeyRef       *instance.KeyRef `json:"keyRef,omitempty"`
		Verification string           `json:"verification"`
	}{Verification: "configuration-only; key access and stored histories not probed"}
	if settings := cfg.TemporalPayloadCodec(); settings != nil {
		status.Enabled, status.Strict, status.KeyRef = settings.KeyRef != nil, settings.Strict, settings.KeyRef
	}
	if format == "json" {
		if err := json.NewEncoder(stdout).Encode(status); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	} else {
		pf(stdout, "Temporal payload codec: %s; strict: %s\n%s\n", onOff(status.Enabled), onOff(status.Strict), status.Verification)
		if status.KeyRef != nil {
			pf(stdout, "Wrapping key: %s/%s (version %q; empty selects current)\n", status.KeyRef.Store, status.KeyRef.Name, status.KeyRef.Version)
		}
	}
	return 0
}
