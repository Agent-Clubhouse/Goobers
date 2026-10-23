package dispatcher

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestSurrenderedOutboxManifestFailsClosed(t *testing.T) {
	valid := SurrenderedOutboxFile{
		RelPath: "reports/result.json",
		Digest:  "sha256:" + strings.Repeat("a", 64),
		Size:    7,
	}
	for _, test := range []struct {
		name  string
		files []SurrenderedOutboxFile
	}{
		{"traversal", []SurrenderedOutboxFile{{RelPath: "../secret", Digest: valid.Digest, Size: 1}}},
		{"backslash", []SurrenderedOutboxFile{{RelPath: `reports\secret`, Digest: valid.Digest, Size: 1}}},
		{"duplicate", []SurrenderedOutboxFile{valid, valid}},
		{"bad digest", []SurrenderedOutboxFile{{RelPath: valid.RelPath, Digest: "sha256:no", Size: 1}}},
		{"oversized", []SurrenderedOutboxFile{{RelPath: valid.RelPath, Digest: valid.Digest, Size: journal.MaxOutboxBytesPerAttempt + 1}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateSurrenderedOutboxManifest(SurrenderContractV3, test.files, true); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
}
