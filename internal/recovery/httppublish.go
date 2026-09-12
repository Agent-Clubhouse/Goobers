package recovery

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPArchivePublisher transfers custody using the source run's claims bearer.
// Success means the daemon acknowledged durable verified custody, not merely
// that a blob was uploaded. The caller retains its source on every error.
type HTTPArchivePublisher HTTPArchiveSource

// PublishArchive streams a bounded verified envelope and requires a complete
// transfer followed by HTTP 204. It neither deletes nor changes archivePath.
func (p HTTPArchivePublisher) PublishArchive(ctx context.Context, issueID string, record Record, archivePath string) error {
	endpoint, err := HTTPArchiveSource(p).endpoint(record.RepositoryKey, issueID)
	if err != nil {
		return err
	}
	if record.RunID != p.RunID || record.ArchiveBytes > 512<<20 {
		return fmt.Errorf("recovery upload identity or size mismatch")
	}
	metadata, err := Encode(record)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return fmt.Errorf("invalid recovery upload request")
	}
	request.ContentLength = 4 + int64(len(metadata)) + record.ArchiveBytes
	request.Header.Set("Authorization", "Bearer "+p.Token)
	request.Header.Set("Content-Type", "application/octet-stream")
	finished := make(chan error, 1)
	go func() {
		err := WriteArchiveEnvelope(ctx, archivePath, record, 512<<20, writer)
		_ = writer.CloseWithError(err)
		finished <- err
	}()
	client := http.Client{}
	if p.Client != nil {
		client = *p.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, transportErr := client.Do(request)
	// Unblock a writer even when the server rejects the headers without reading
	// the body. Join it on every exit; no upload goroutine may outlive its source.
	_ = reader.Close()
	writeErr := <-finished
	if response != nil {
		_ = response.Body.Close()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if transportErr != nil || writeErr != nil {
		// Do not expose URL-bearing transport errors or private archive paths.
		return fmt.Errorf("recovery upload transfer failed")
	}
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("recovery upload refused (HTTP %d)", response.StatusCode)
	}
	return nil
}
