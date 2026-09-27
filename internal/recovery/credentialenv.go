package recovery

import (
	"fmt"
	"strings"
)

// adoMsaPassThroughHeader is the only value recovery accepts in the third Git
// configuration slot. It must match what providers.ADOGitAuthEnvironment emits
// for a bearer credential.
const adoMsaPassThroughHeader = "X-VSS-ForceMsaPassThrough: true"

// validateCredentialEnvironment permits the authentication fields emitted by
// credentials.GitAuthEnvironment and providers.ADOGitAuthEnvironment without
// allowing repository/object redirection or arbitrary Git configuration. The
// accepted GIT_CONFIG_* shapes are exactly:
//   - COUNT=1: a disabled credential.helper;
//   - COUNT=2: that plus one authorization extraheader scoped to remoteURL
//     (GitHub, Gitea, ADO PAT);
//   - COUNT=3: that plus a second extraheader on the same scope carrying only
//     X-VSS-ForceMsaPassThrough (ADO bearer credentials).
//
// Never include environment values in errors.
func validateCredentialEnvironment(environment []string, remoteURL string) error {
	config, err := collectCredentialConfig(environment)
	if err != nil {
		return err
	}
	if len(config) == 0 {
		return nil
	}
	unsupported := fmt.Errorf("recovery credentials contain unsupported Git configuration")
	if config["GIT_CONFIG_KEY_0"] != "credential.helper" || config["GIT_CONFIG_VALUE_0"] != "" {
		return unsupported
	}
	if len(config) == 3 && config["GIT_CONFIG_COUNT"] == "1" {
		return nil
	}
	scopedHeader := "http." + strings.TrimSuffix(remoteURL, "/") + "/.extraheader"
	if remoteURL == "" || config["GIT_CONFIG_KEY_1"] != scopedHeader || strings.ContainsAny(config["GIT_CONFIG_VALUE_1"], "\r\n\x00") {
		return unsupported
	}
	if len(config) == 5 && config["GIT_CONFIG_COUNT"] == "2" {
		return nil
	}
	if len(config) == 7 && config["GIT_CONFIG_COUNT"] == "3" &&
		config["GIT_CONFIG_KEY_2"] == scopedHeader && config["GIT_CONFIG_VALUE_2"] == adoMsaPassThroughHeader {
		return nil
	}
	return unsupported
}

// collectCredentialConfig returns the GIT_CONFIG_* entries of environment and
// rejects every other Git override except the askpass and prompt controls.
func collectCredentialConfig(environment []string) (map[string]string, error) {
	config := make(map[string]string)
	seen := make(map[string]bool)
	for _, entry := range environment {
		key, value, present := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if !strings.HasPrefix(key, "GIT_") {
			continue
		}
		if !present || seen[key] {
			return nil, fmt.Errorf("invalid or duplicate recovery Git credential field")
		}
		seen[key] = true
		switch key {
		case "GIT_ASKPASS", "GIT_TERMINAL_PROMPT":
		case "GIT_CONFIG_COUNT",
			"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
			"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1",
			"GIT_CONFIG_KEY_2", "GIT_CONFIG_VALUE_2":
			config[key] = value
		default:
			return nil, fmt.Errorf("recovery credentials contain a non-authentication Git override")
		}
	}
	return config, nil
}
