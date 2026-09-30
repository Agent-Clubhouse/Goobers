package telemetry

import (
	"compress/gzip"
	"errors"
	"io"
)

// Retain a bounded number of compressors, not payload buffers. Cache misses
// allocate rather than waiting for another request. This avoids allocating
// gzip's approximately 800 KiB workspace on every small, sparse upload while
// keeping idle retention bounded across all clients in this process.
var azureMonitorCompression = newAzureGzipCache(4)

type azureGzipCache struct {
	idle chan *gzip.Writer
}

func newAzureGzipCache(capacity int) *azureGzipCache {
	return &azureGzipCache{idle: make(chan *gzip.Writer, capacity)}
}

func (c *azureGzipCache) take(dst io.Writer) *gzip.Writer {
	select {
	case writer := <-c.idle:
		writer.Reset(dst)
		return writer
	default:
		return gzip.NewWriter(dst)
	}
}

func (c *azureGzipCache) release(writer *gzip.Writer) {
	// Break the reference to this request's output buffer before caching. The
	// HTTP request owns that buffer independently until its upload finishes.
	writer.Reset(io.Discard)
	select {
	case c.idle <- writer:
	default:
	}
}

func (c *azureGzipCache) compress(dst io.Writer, raw []byte) error {
	writer := c.take(dst)
	defer c.release(writer)
	_, writeErr := writer.Write(raw)
	return errors.Join(writeErr, writer.Close())
}
