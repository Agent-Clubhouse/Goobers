package recovery

import (
	"fmt"
	"strings"
)

// validateCredentialEnvironment permits the authentication fields emitted by
// credentials.GitAuthEnvironment without allowing repository/object redirection or
// arbitrary Git configuration. Never include environment values in errors.
func validateCredentialEnvironment(environment []string, remoteURL string) error {
	config := make(map[string]string)
	seen := make(map[string]bool)
	for _, entry := range environment {
		key, value, present := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if !strings.HasPrefix(key, "GIT_") {
			continue
		}
		if !present || seen[key] {
			return fmt.Errorf("invalid or duplicate recovery Git credential field")
		}
		seen[key] = true
		switch key {
		case "GIT_ASKPASS", "GIT_TERMINAL_PROMPT":
		case "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1":
			config[key] = value
		default:
			return fmt.Errorf("recovery credentials contain a non-authentication Git override")
		}
	}
	if len(config) == 0 {
		return nil
	}
	if config["GIT_CONFIG_KEY_0"] != "credential.helper" || config["GIT_CONFIG_VALUE_0"] != "" {
		return fmt.Errorf("recovery credentials contain unsupported Git configuration")
	}
	if len(config) == 3 && config["GIT_CONFIG_COUNT"] == "1" {
		return nil
	}
	scopedHeader := "http." + strings.TrimSuffix(remoteURL, "/") + "/.extraheader"
	if remoteURL != "" && len(config) == 5 && config["GIT_CONFIG_COUNT"] == "2" && config["GIT_CONFIG_KEY_1"] == scopedHeader && !strings.ContainsAny(config["GIT_CONFIG_VALUE_1"], "\r\n\x00") {
		return nil
	}
	return fmt.Errorf("recovery credentials contain unsupported Git configuration")
}
