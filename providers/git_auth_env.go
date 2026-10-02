package providers

import (
	"os"
	"strings"
)

func scopedGitExtraHeaderEnv(remoteURL, header string, registrar SecretRegistrar, scrubForms ...string) []string {
	base := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if upper == "GIT_CONFIG_COUNT" || upper == "GIT_TERMINAL_PROMPT" ||
			strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_") {
			continue
		}
		base = append(base, entry)
	}
	if strings.TrimSpace(header) == "" {
		return append(base, "GIT_TERMINAL_PROMPT=0")
	}
	if registrar != nil {
		for _, form := range scrubForms {
			registrar.Register([]byte(form))
		}
	}
	scopedURL := strings.TrimRight(remoteURL, "/") + "/"
	return append(base,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=http."+scopedURL+".extraheader",
		"GIT_CONFIG_VALUE_1=AUTHORIZATION: "+header,
		"GIT_TERMINAL_PROMPT=0",
	)
}
