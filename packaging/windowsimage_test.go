package packaging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWindowsImagePinnedInputs(t *testing.T) {
	var dependencies struct {
		WindowsBase    string                       `json:"windowsBase"`
		WindowsVersion string                       `json:"windowsVersion"`
		MinGit         struct{ URL, SHA256 string } `json:"mingit"`
		ZoneInfo       struct{ URL, SHA256 string } `json:"zoneinfo"`
	}
	if err := json.Unmarshal(readWindowsImageFile(t, "dependencies.json"), &dependencies); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^mcr\.microsoft\.com/windows/servercore:ltsc2022@sha256:[a-f0-9]{64}$`).MatchString(dependencies.WindowsBase) || !strings.HasPrefix(dependencies.WindowsVersion, "10.0.20348.") {
		t.Fatalf("unqualified Windows Server 2022 base: %+v", dependencies)
	}
	dockerfile := string(readWindowsImageFile(t, "Dockerfile"))
	if !strings.Contains(dockerfile, "ARG WINDOWS_BASE_IMAGE="+dependencies.WindowsBase+"\n") {
		t.Fatal("Dockerfile and dependency manifest base pins disagree")
	}
	for name, hash := range map[string]string{"mingit": dependencies.MinGit.SHA256, "zoneinfo": dependencies.ZoneInfo.SHA256} {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(hash) {
			t.Errorf("%s checksum is not pinned: %q", name, hash)
		}
	}
	if !strings.HasPrefix(dependencies.MinGit.URL, "https://github.com/git-for-windows/git/releases/download/") || !strings.HasSuffix(dependencies.MinGit.URL, "-64-bit.zip") || strings.Contains(dependencies.MinGit.URL, "busybox") {
		t.Fatalf("expected regular upstream x64 MinGit: %s", dependencies.MinGit.URL)
	}
	goMod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	version := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(string(goMod))
	if len(version) != 2 || dependencies.ZoneInfo.URL != "https://raw.githubusercontent.com/golang/go/go"+version[1]+"/lib/time/zoneinfo.zip" {
		t.Fatalf("zoneinfo source must match repository Go toolchain: %s", dependencies.ZoneInfo.URL)
	}
	user := strings.LastIndex(dockerfile, "\nUSER ContainerUser\n")
	verify := strings.LastIndex(dockerfile, "\nRUN C:\\Goobers\\Verify-Image.ps1\n")
	if user < 0 || verify < user {
		t.Fatal("native contract must execute after switching to ContainerUser")
	}
	for _, forbidden := range []string{"COPY . ", "ADD ", "Invoke-WebRequest", "curl ", "npm ", "choco "} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("prepared image Dockerfile contains %q", forbidden)
		}
	}
}

func readWindowsImageFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("docker/windows", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
