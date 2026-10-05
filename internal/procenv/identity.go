package procenv

import (
	"os"
	"path/filepath"
)

// IsolatedIdentityEnvironment retains only process bootstrap settings and
// redirects credential/config discovery to a launcher-owned empty directory.
// Callers add declared credentials afterward. This does not promise isolation
// from arbitrary host file reads; the caller must enforce its sandbox policy.
func IsolatedIdentityEnvironment(home string) []string {
	result := []string{}
	for _, key := range []string{"PATH", "LANG", "SystemRoot", "WINDIR", "ComSpec", "PATHEXT"} {
		if value := os.Getenv(key); value != "" {
			result = append(result, key+"="+value)
		}
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "GH_CONFIG_DIR", "AZURE_CONFIG_DIR", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "DOCKER_CONFIG", "CLOUDSDK_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		result = append(result, key+"="+filepath.Join(home, key))
	}
	// HOME itself must name the writable home root, not a nonexistent child.
	result = append(result, "HOME="+home, "USERPROFILE="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "SSH_AUTH_SOCK=")
	return result
}
