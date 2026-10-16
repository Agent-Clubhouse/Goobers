package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// publishClientMargin sits on top of the server's budget plus its 5s
// write-deadline margin (httpapi.writeDeadlineMargin), so the server decides
// first and can answer before the client gives up.
const publishClientMargin = 15 * time.Second

// PublishTimeout bounds one archive upload on the client. It exceeds the
// route's server-side budget plus the server's write-deadline margin, so a
// slow upload is cut by the server with a diagnosable failure rather than by
// the client first.
const PublishTimeout = apicontract.RecoveryPublishBudget + publishClientMargin

// DownloadTimeout bounds one archive download on the client, above the route's
// server budget plus write-deadline margin for the same reason as PublishTimeout.
const DownloadTimeout = apicontract.RecoveryDownloadBudget + publishClientMargin

// countingWriter counts bytes the HTTP transport has consumed through the
// upload pipe.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// transferFailure describes a failed upload without URLs or paths: a coarse
// cause class, elapsed time and byte counts.
func transferFailure(cause string, elapsed time.Duration, sent, total int64) error {
	return fmt.Errorf("recovery upload transfer failed (%s after %s, %d of %d bytes sent)",
		cause, elapsed.Round(time.Millisecond), sent, total)
}

// classifyTransport reduces a transport error to a class that cannot carry a
// URL or path. The error text itself is never returned.
func classifyTransport(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "client timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "server closed the stream"
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "network timeout"
	}
	return "transport error"
}

// PublicationRefusal preserves the API's bounded public refusal class without
// exposing response detail, URLs, credentials, or archive paths.
type PublicationRefusal struct {
	StatusCode int
	Code       string
}

func (e *PublicationRefusal) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("recovery upload refused (HTTP %d)", e.StatusCode)
	}
	return fmt.Sprintf("recovery upload refused (%s, HTTP %d)", e.Code, e.StatusCode)
}

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
	if record.RunID != p.RunID || record.ArchiveBytes > apicontract.RecoveryArchiveMaxBytes {
		return fmt.Errorf("recovery upload identity or size mismatch")
	}
	metadata, err := Encode(record)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, PublishTimeout)
	started := time.Now()
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
	sent := &countingWriter{w: writer}
	finished := make(chan error, 1)
	go func() {
		err := WriteArchiveEnvelope(ctx, archivePath, record, apicontract.RecoveryArchiveMaxBytes, sent)
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
	var refusalCode string
	if response != nil {
		if response.StatusCode != http.StatusNoContent {
			var envelope apicontract.ErrorEnvelope
			if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&envelope); err == nil {
				refusalCode = envelope.Error.Code
			}
		}
		_ = response.Body.Close()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("recovery upload stopped after %s, %d of %d bytes sent: %w",
			time.Since(started).Round(time.Millisecond), sent.n, request.ContentLength, err)
	}
	// A server refusal is more authoritative than the pipe error caused when
	// that server stops reading the upload. Preserve its public cause class.
	if response != nil && response.StatusCode != http.StatusNoContent {
		return &PublicationRefusal{StatusCode: response.StatusCode, Code: refusalCode}
	}
	if transportErr != nil || writeErr != nil {
		// Do not expose URL-bearing transport errors or private archive paths:
		// report only a coarse class, elapsed time and byte counts.
		cause := "archive read or envelope write error"
		if transportErr != nil {
			cause = classifyTransport(transportErr)
		}
		return transferFailure(cause, time.Since(started), sent.n, request.ContentLength)
	}
	return nil
}
