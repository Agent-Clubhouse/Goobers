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
	want := "Dir::Etc::sourcelist \"/etc/apt/sources.list.d/ubuntu.sources\";\nDir::Etc::sourceparts \"-\";"
	if strings.Join(settings, "\n") != want {
		t.Fatalf("apt config must only select sources, without weakening authentication: %q", settings)
	}
}
