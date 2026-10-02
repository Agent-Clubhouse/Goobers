//go:build unix

package configauthoring

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"golang.org/x/sys/unix"
)

func TestLocalReaderRejectsNonRegularDocuments(t *testing.T) {
	root := testSource(t)
	const logicalPath = "blocked.yaml"
	if err := unix.Mkfifo(filepath.Join(root, logicalPath), 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	reader, err := NewReader(root, apicontract.ConfigSourceLocal, true)
	if err != nil {
		t.Fatal(err)
	}

	page, err := reader.Documents(context.Background(), localSourceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range page.Items {
		if document.Path == logicalPath {
			t.Fatalf("Documents() exposed non-regular path %q", logicalPath)
		}
	}
	if _, err := reader.Document(context.Background(), localSourceID, logicalPath); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("Document() error = %v, want ErrDocumentNotFound", err)
	}
}
