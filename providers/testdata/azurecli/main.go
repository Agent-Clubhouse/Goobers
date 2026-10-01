package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Synthetic Azure CLI: never contacts Azure. Failure modes print the shapes of
// real `az account get-access-token` failures next to sensitive-looking
// canaries, so tests prove the caller classifies them without echoing them.
func main() {
	if len(os.Args) < 3 || os.Args[1] != "-IBm" || os.Args[2] != "azure.cli" {
		os.Exit(2)
	}
	args, err := json.Marshal(os.Args[1:])
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("GOOBERS_AZURE_CLI_FIXTURE_ARGS"), args, 0o600); err != nil {
		os.Exit(2)
	}
	mode := os.Getenv("GOOBERS_AZURE_CLI_FIXTURE_MODE")
	switch mode {
	case "failure", "login", "account", "offline":
		_, _ = fmt.Fprintln(os.Stdout, `{"accessToken":"fixture-sensitive-stdout"}`)
		_, _ = fmt.Fprintln(os.Stderr, "Authorization: Bearer fixture-sensitive-stderr")
		switch mode {
		case "login":
			_, _ = fmt.Fprintln(os.Stderr, "ERROR: AADSTS700082: The refresh token has expired due to inactivity."+
				" Trace ID: fixture-sensitive-trace")
			_, _ = fmt.Fprintln(os.Stderr, "Interactive authentication is needed. Please run:")
			_, _ = fmt.Fprintln(os.Stderr, "az login --scope 499b84ac-1321-427f-aa17-267ca6975798/.default")
			os.Exit(1)
		case "account":
			_, _ = fmt.Fprintln(os.Stderr, "ERROR: Please run 'az login' to setup account.")
			os.Exit(1)
		case "offline":
			_, _ = fmt.Fprintln(os.Stderr, "ERROR: HTTPSConnectionPool(host='login.microsoftonline.com', port=443):"+
				" Max retries exceeded with url: /fixture-sensitive-tenant/oauth2/v2.0/token"+
				" (Caused by NewConnectionError('<urllib3.connection.HTTPSConnection object>:"+
				" Failed to establish a new connection: [Errno 8] nodename nor servname provided, or not known'))")
			os.Exit(1)
		}
		os.Exit(7)
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
		"accessToken": "fixture-success-token",
		"expires_on":  time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		os.Exit(2)
	}
}
