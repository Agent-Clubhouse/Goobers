package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDistroDependencyJobsUseUbuntuSources(t *testing.T) {
	w := loadCIWorkflow(t)
	for job, name := range map[string]string{
		"checks":  "Install Portal dependencies and pinned Chromium",
		"sandbox": "Install bubblewrap",
	} {
		t.Run(job, func(t *testing.T) {
			script := w.Jobs[job].step(t, name).Run
			guard := strings.Index(script, "test -s /etc/apt/sources.list.d/ubuntu.sources")
			install := strings.Index(script, "sudo install -m 0644 .github/apt/ubuntu-only.conf /etc/apt/apt.conf.d/99goobers-ubuntu-only")
			if guard < 0 || install <= guard {
				t.Fatal("distro-only apt configuration must require the existing Ubuntu sources")
			}
			for _, unsafe := range []string{"--allow-unauthenticated", "AllowInsecureRepositories", "trusted=yes"} {
				if strings.Contains(script, unsafe) {
					t.Fatalf("dependency setup disables apt verification: %s", unsafe)
				}
			}
			if !strings.Contains(script, "for attempt in 1 2 3") {
				t.Fatal("dependency setup must retain bounded retries")
			}
		})
	}
	t.Run("checks touches apt only for missing Chromium libraries", func(t *testing.T) {
		script := w.Jobs["checks"].step(t, "Install Portal dependencies and pinned Chromium").Run
		if strings.Contains(script, "--with-deps") {
			t.Fatal("checks must not unconditionally install Chromium system packages; hosted images usually ship them")
		}
		browser := strings.Index(script, "playwright install chromium")
		gate := strings.Index(script, "ldd $browser_bins 2>&1 | grep -q 'not found'; then")
		wait := strings.Index(script, "for _ in $(seq 1 180); do")
		probe := strings.Index(script, "pgrep -x 'apt|apt-.*|dpkg|unattended-upgr' >/dev/null || break")
		deps := strings.Index(script, "playwright install-deps chromium")
		if browser < 0 || gate <= browser || wait <= gate || probe <= wait || deps <= probe {
			t.Fatal("checks must install deps only when ldd reports a gap, after a finite wait for runner apt/dpkg work to exit")
		}
		if !strings.Contains(script, `[ -z "$browser_bins" ] ||`) {
			t.Fatal("checks must fail closed and install deps when no Chromium binary is found")
		}
	})
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "apt", "ubuntu-only.conf"))
	if err != nil {
		t.Fatal(err)
	}
	var settings []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "//") {
			settings = append(settings, line)
		}
	}
	want := "Dir::Etc::sourcelist \"/etc/apt/sources.list.d/ubuntu.sources\";\nDir::Etc::sourceparts \"-\";\nDPkg::Lock::Timeout \"60\";"
	if strings.Join(settings, "\n") != want {
		t.Fatalf("apt config must only select sources and wait for the dpkg lock, without weakening authentication: %q", settings)
	}
}
