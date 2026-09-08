package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Tests replace the source directory and client with isolated pinned materials
// and a local TLS server; ordinary releases use only the committed public pins.
var (
	windowsImageSourceDirectory = filepath.Join("packaging", "docker", "windows")
	windowsImageHTTPClient      = &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many image dependency redirects")
			}
			if request.URL.Scheme != "https" || request.URL.User != nil {
				return fmt.Errorf("image dependency redirect must use HTTPS without credentials")
			}
			return nil
		},
	}
)

var windowsImageSourceFiles = []string{
	"Dockerfile", ".dockerignore", "dependencies.json", "Verify-Inputs.ps1",
	"Configure-Image.ps1", "Verify-Image.ps1", "Release-Metadata.ps1", "Shell-Contract.sh",
}

type imageDependency struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type windowsImageDependencies struct {
	WindowsBase    string          `json:"windowsBase"`
	WindowsVersion string          `json:"windowsVersion"`
	MinGit         imageDependency `json:"mingit"`
	ZoneInfo       imageDependency `json:"zoneinfo"`
}

func windowsImageMaterials(repoRoot string, targets []Target) (map[string][]byte, error) {
	for _, target := range targets {
		if target.OS == "windows" {
			return readWindowsImageMaterials(repoRoot)
		}
	}
	return nil, nil
}

func readWindowsImageMaterials(repoRoot string) (map[string][]byte, error) {
	directory := windowsImageSourceDirectory
	if !filepath.IsAbs(directory) {
		directory = filepath.Join(repoRoot, directory)
	}
	files := make(map[string][]byte)
	for _, name := range windowsImageSourceFiles {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return nil, fmt.Errorf("read Windows image input %s: %w", name, err)
		}
		files[name] = data
	}
	// Refuse malformed dependency pins before any build or download work.
	if _, err := parseWindowsImageDependencies(files["dependencies.json"]); err != nil {
		return nil, err
	}
	return files, nil
}

func parseWindowsImageDependencies(data []byte) (windowsImageDependencies, error) {
	var dependencies windowsImageDependencies
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&dependencies); err != nil {
		return dependencies, fmt.Errorf("decode Windows image dependency pins: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return dependencies, fmt.Errorf("image dependency pins for Windows must contain exactly one JSON object")
	}
	for _, dependency := range []imageDependency{dependencies.MinGit, dependencies.ZoneInfo} {
		if err := validateImageDependency(dependency); err != nil {
			return dependencies, err
		}
	}
	return dependencies, nil
}

func validateImageDependency(dependency imageDependency) error {
	parsed, err := url.Parse(dependency.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("image dependency source must be a public HTTPS URL without credentials, query or fragment")
	}
	digest, err := hex.DecodeString(dependency.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("image dependency requires a SHA256 digest")
	}
	return nil
}

func prepareWindowsImageDependencies(directory string, data []byte) error {
	dependencies, err := parseWindowsImageDependencies(data)
	if err != nil {
		return err
	}
	for _, input := range []struct {
		name       string
		dependency imageDependency
		maxBytes   int64
	}{
		{"mingit.zip", dependencies.MinGit, 128 << 20},
		{"zoneinfo.zip", dependencies.ZoneInfo, 4 << 20},
	} {
		if err := downloadImageDependency(windowsImageHTTPClient, directory, input.name, input.dependency, input.maxBytes); err != nil {
			return err
		}
	}
	return nil
}

func downloadImageDependency(client *http.Client, directory, name string, dependency imageDependency, maxBytes int64) error {
	if err := validateImageDependency(dependency); err != nil {
		return err
	}
	response, err := client.Get(dependency.URL)
	if err != nil {
		return fmt.Errorf("download image dependency %s: %w", name, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download image dependency %s: HTTP %d", name, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("read image dependency %s: %w", name, err)
	}
	if int64(len(data)) > maxBytes {
		return fmt.Errorf("image dependency %s exceeds %d-byte limit", name, maxBytes)
	}
	expected, _ := hex.DecodeString(dependency.SHA256) // validated before download
	actual := sha256.Sum256(data)
	if !bytes.Equal(actual[:], expected) {
		return fmt.Errorf("image dependency %s SHA256 mismatch", name)
	}
	if err := os.WriteFile(filepath.Join(directory, name), data, 0o644); err != nil {
		return fmt.Errorf("write image dependency %s: %w", name, err)
	}
	return nil
}
