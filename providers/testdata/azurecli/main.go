package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

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
	switch os.Getenv("GOOBERS_AZURE_CLI_FIXTURE_MODE") {
	case "failure", "login":
		_, _ = fmt.Fprintln(os.Stdout, `{"accessToken":"fixture-sensitive-stdout"}`)
		_, _ = fmt.Fprintln(os.Stderr, "Authorization: Bearer fixture-sensitive-stderr")
		if os.Getenv("GOOBERS_AZURE_CLI_FIXTURE_MODE") == "login" {
			_, _ = fmt.Fprintln(os.Stderr, "Please run 'az login' to setup account.")
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
