package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/goobers/goobers/internal/fleet"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/platform/secfile"
)

const fleetHelp = `Usage: goobers fleet <join|status|leave> [flags] [path]

Associate a local Goobers instance with a Fleet service, inspect its durable
connection state, or remove the association. Fleet identity and credentials
are stored outside the instance root so copying an instance does not clone its
identity.
`

const fleetJoinHelp = `Usage: goobers fleet join --url <url> [--enrollment-token-file <path>] [--grant-local-admin | --no-grant-local-admin] [path]

Discover and enroll an instance with a Fleet service. By default the one-time
enrollment grant is read from a protected terminal prompt and never accepted
as a command-line value. --enrollment-token-file supports automation and is
accepted only when the file is private to its owner.

When discovery advertises a local administrator principal, interactive use
offers an explicit instance:read self-grant. Noninteractive use must choose
--grant-local-admin or --no-grant-local-admin. An empty ACL always requires an
explicit opt-out or interactive confirmation.
`

const fleetStatusHelp = `Usage: goobers fleet status [--json] [path]

Show the durable Fleet registration, connection, heartbeat, ACL version, and
credential expiry state. Private key and bearer credential material are never
printed.
`

const fleetLeaveHelp = `Usage: goobers fleet leave [path]

Remove the Fleet association, private key, and bearer credential. A running
daemon observes the removal and stops reconnecting.
`

var (
	newFleetStorage = func() (fleet.Storage, error) {
		return fleet.NewFileStorage("")
	}
	fleetHTTPClient = &http.Client{Timeout: 15 * time.Second}
)

func runFleet(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		pf(stdout, "%s", fleetHelp)
		return 0
	}
	pf(stderr, "%s", fleetHelp)
	return 2
}

func runFleetJoin(args []string, stdout, stderr io.Writer) int {
	return runFleetJoinWithInput(context.Background(), args, os.Stdin, stdout, stderr, stdinIsTerminal(os.Stdin))
}

func runFleetJoinWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	fs := newCLIFlagSet("fleet join", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "fleet join")
	fleetURL := fs.String("url", "", "Fleet service URL")
	tokenFile := fs.String("enrollment-token-file", "", "private file containing the one-time enrollment grant")
	grantLocalAdmin := fs.Bool("grant-local-admin", false, "grant the discovered local administrator instance:read")
	noGrantLocalAdmin := fs.Bool("no-grant-local-admin", false, "explicitly enroll with an empty ACL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *fleetURL == "" {
		pf(stderr, "error: --url is required\n")
		return 2
	}
	if *grantLocalAdmin && *noGrantLocalAdmin {
		pf(stderr, "error: --grant-local-admin and --no-grant-local-admin cannot be combined\n")
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	if err := requireFleetInstanceRoot(root); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	storage, err := newFleetStorage()
	if err != nil {
		pf(stderr, "error: initialize Fleet storage: %v\n", err)
		return 2
	}
	if _, err := storage.Load(root); err == nil {
		pf(stderr, "error: instance is already associated with a Fleet; run `goobers fleet leave` first\n")
		return 1
	} else if !errors.Is(err, fleet.ErrNotAssociated) {
		pf(stderr, "error: read Fleet association: %v\n", err)
		return 1
	}

	client := fleet.Client{HTTP: fleetHTTPClient}
	discovery, err := client.Discover(ctx, *fleetURL)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	acl, err := selectFleetACL(discovery, *grantLocalAdmin, *noGrantLocalAdmin, interactive, stdin, stdout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	grant, err := readEnrollmentGrant(*tokenFile, stdin, stdout, interactive)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	canonicalRoot, err := fleet.CanonicalInstanceRoot(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	association, err := client.JoinDiscovered(ctx, storage, discovery, fleet.JoinOptions{
		URL:          *fleetURL,
		Grant:        grant,
		InstanceRoot: root,
		DisplayName:  filepath.Base(canonicalRoot),
		ACL:          acl,
	})
	if err != nil {
		pf(stderr, "error: enroll instance: %v\n", err)
		return 1
	}
	pf(stdout, "joined Fleet %s as instance %s (registration %s)\n",
		association.CanonicalURI, association.InstanceID, association.RegistrationID)
	return 0
}

func runFleetStatus(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("fleet status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "fleet status")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	if err := requireFleetInstanceRoot(root); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	storage, err := newFleetStorage()
	if err != nil {
		pf(stderr, "error: initialize Fleet storage: %v\n", err)
		return 2
	}
	record, err := storage.Load(root)
	if errors.Is(err, fleet.ErrNotAssociated) {
		if *asJSON {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"associated": false,
			})
		} else {
			pln(stdout, "Fleet: not associated")
		}
		return 0
	}
	if err != nil {
		pf(stderr, "error: read Fleet association: %v\n", err)
		return 1
	}
	status := fleetStatusOutput{
		Associated:             true,
		InstanceID:             record.Association.InstanceID,
		DisplayName:            record.Association.DisplayName,
		FleetID:                record.Association.FleetID,
		RegistrationID:         record.Association.RegistrationID,
		RegistrationGeneration: record.Association.RegistrationGeneration,
		CanonicalURI:           record.Association.CanonicalURI,
		ConnectionEndpoint:     record.Association.ConnectionEndpoint,
		ProtocolVersion:        record.Association.ProtocolVersion,
		ACLVersion:             record.Association.ACL.PolicyVersion,
		CredentialExpiresAt:    record.Association.CredentialExpiresAt,
		Revoked:                record.Association.Revoked,
		RevokeReason:           record.Association.RevokeReason,
		Connected:              record.Association.Connected,
		ConnectionID:           record.Association.ConnectionID,
		LastConnectedAt:        record.Association.LastConnectedAt,
		LastHeartbeatAt:        record.Association.LastHeartbeatAt,
		LastError:              record.Association.LastError,
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(status); err != nil {
			pf(stderr, "error: encode Fleet status: %v\n", err)
			return 2
		}
		return 0
	}
	pf(stdout, "Fleet: %s\n", status.CanonicalURI)
	pf(stdout, "  instance: %s (%s)\n", status.DisplayName, status.InstanceID)
	pf(stdout, "  registration: %s generation %d\n", status.RegistrationID, status.RegistrationGeneration)
	pf(stdout, "  connection: %s", fleetConnectionState(status))
	if status.ConnectionID != "" {
		pf(stdout, " (%s)", status.ConnectionID)
	}
	pln(stdout, "")
	pf(stdout, "  heartbeat: %s\n", formatFleetTime(status.LastHeartbeatAt))
	pf(stdout, "  ACL version: %s\n", status.ACLVersion)
	pf(stdout, "  credential expires: %s\n", formatFleetTime(status.CredentialExpiresAt))
	if status.Revoked {
		pf(stdout, "  revoked: %s\n", status.RevokeReason)
	}
	if status.LastError != "" {
		pf(stdout, "  last error: %s\n", status.LastError)
	}
	return 0
}

func runFleetLeave(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("fleet leave", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "fleet leave")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	if err := requireFleetInstanceRoot(root); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	storage, err := newFleetStorage()
	if err != nil {
		pf(stderr, "error: initialize Fleet storage: %v\n", err)
		return 2
	}
	if err := storage.Delete(root); err != nil {
		if errors.Is(err, fleet.ErrNotAssociated) {
			pf(stderr, "error: instance is not associated with a Fleet\n")
			return 1
		}
		pf(stderr, "error: remove Fleet association: %v\n", err)
		return 1
	}
	pln(stdout, "left Fleet; removed association, private key, and credential")
	return 0
}

type fleetStatusOutput struct {
	Associated             bool      `json:"associated"`
	InstanceID             string    `json:"instanceId,omitempty"`
	DisplayName            string    `json:"displayName,omitempty"`
	FleetID                string    `json:"fleetId,omitempty"`
	RegistrationID         string    `json:"registrationId,omitempty"`
	RegistrationGeneration int64     `json:"registrationGeneration,omitempty"`
	CanonicalURI           string    `json:"canonicalUri,omitempty"`
	ConnectionEndpoint     string    `json:"connectionEndpoint,omitempty"`
	ProtocolVersion        string    `json:"protocolVersion,omitempty"`
	ACLVersion             string    `json:"aclVersion,omitempty"`
	CredentialExpiresAt    time.Time `json:"credentialExpiresAt,omitempty"`
	Revoked                bool      `json:"revoked"`
	RevokeReason           string    `json:"revokeReason,omitempty"`
	Connected              bool      `json:"connected"`
	ConnectionID           string    `json:"connectionId,omitempty"`
	LastConnectedAt        time.Time `json:"lastConnectedAt,omitempty"`
	LastHeartbeatAt        time.Time `json:"lastHeartbeatAt,omitempty"`
	LastError              string    `json:"lastError,omitempty"`
}

func requireFleetInstanceRoot(root string) error {
	path := instance.NewLayout(root).ConfigFile()
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s not found (not an instance root - run `goobers init` first): %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is not a file", path)
	}
	return nil
}

func selectFleetACL(discovery fleet.Discovery, grant, noGrant, interactive bool, stdin io.Reader, stdout io.Writer) (fleet.ACL, error) {
	acl := fleet.ACL{PolicyVersion: fleet.ProtocolVersion, Grants: []fleet.Grant{}}
	principal := discovery.LocalAdministratorPrincipal
	if grant {
		if principal == nil {
			return fleet.ACL{}, fmt.Errorf("--grant-local-admin was specified but Fleet discovery did not advertise a local administrator principal")
		}
		return fleet.ACL{
			PolicyVersion: fleet.ProtocolVersion,
			Grants: []fleet.Grant{{
				Principal:    *principal,
				Capabilities: []string{"instance:read"},
			}},
		}, nil
	}
	if noGrant {
		return acl, nil
	}
	if !interactive {
		return fleet.ACL{}, fmt.Errorf("noninteractive join requires --grant-local-admin or --no-grant-local-admin")
	}
	reader := bufio.NewReader(stdin)
	if principal != nil {
		pf(stdout, "Grant %s (%s) instance:read access? [Y/n]: ", principal.Subject, principal.Issuer)
		answer, err := readFleetAnswer(reader)
		if err != nil {
			return fleet.ACL{}, err
		}
		if answer == "" || answer == "y" || answer == "yes" {
			acl.Grants = append(acl.Grants, fleet.Grant{
				Principal:    *principal,
				Capabilities: []string{"instance:read"},
			})
			return acl, nil
		}
		if answer != "n" && answer != "no" {
			return fleet.ACL{}, fmt.Errorf("local administrator grant response must be yes or no")
		}
	}
	pf(stdout, "WARNING: enrolling with an empty ACL grants no Fleet user access. Continue? [y/N]: ")
	answer, err := readFleetAnswer(reader)
	if err != nil {
		return fleet.ACL{}, err
	}
	if answer != "y" && answer != "yes" {
		return fleet.ACL{}, fmt.Errorf("empty ACL enrollment was not confirmed")
	}
	return acl, nil
}

func readFleetAnswer(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read confirmation: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(line)), nil
}

func readEnrollmentGrant(tokenFile string, stdin io.Reader, stdout io.Writer, interactive bool) (string, error) {
	if tokenFile != "" {
		if err := secfile.VerifyPrivate(tokenFile); err != nil {
			return "", fmt.Errorf("enrollment token file: %w", err)
		}
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("read enrollment token file: %w", err)
		}
		grant := strings.TrimSpace(string(data))
		if grant == "" {
			return "", fmt.Errorf("enrollment token file is empty")
		}
		return grant, nil
	}
	if !interactive {
		return "", fmt.Errorf("a protected terminal is required unless --enrollment-token-file is used")
	}
	file, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return "", fmt.Errorf("a protected terminal is required unless --enrollment-token-file is used")
	}
	pf(stdout, "Enrollment grant: ")
	secret, err := term.ReadPassword(int(file.Fd()))
	pln(stdout, "")
	if err != nil {
		return "", fmt.Errorf("read enrollment grant: %w", err)
	}
	grant := strings.TrimSpace(string(secret))
	for i := range secret {
		secret[i] = 0
	}
	if grant == "" {
		return "", fmt.Errorf("enrollment grant must not be empty")
	}
	return grant, nil
}

func stdinIsTerminal(stdin *os.File) bool {
	return term.IsTerminal(int(stdin.Fd()))
}

func fleetConnectionState(status fleetStatusOutput) string {
	switch {
	case status.Revoked:
		return "revoked"
	case status.Connected:
		return "connected"
	default:
		return "disconnected"
	}
}

func formatFleetTime(value time.Time) string {
	if value.IsZero() {
		return "never"
	}
	return value.UTC().Format(time.RFC3339)
}
