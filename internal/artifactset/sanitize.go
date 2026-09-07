package artifactset

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

// Scrubber is the run's configured secret-redaction policy.
type Scrubber interface{ Scrub([]byte) []byte }

// NewSanitizer supplies the production text/JSON/reproduction-archive policy.
// Containers are decoded before redaction, then deterministically rebuilt with
// no host metadata. Opaque binary formats fail closed, not through a byte scrub
// that could miss encoded credentials or corrupt their structure.
func NewSanitizer(scrubber Scrubber) Sanitize {
	return func(media string, data []byte) ([]byte, error) {
		if scrubber == nil || len(data) > MaxPayloadBytes {
			return nil, errors.New("missing scrubber or oversized payload")
		}
		switch media {
		case "text/plain", "text/markdown", "text/x-patch", "text/x-diff", "application/json":
			return sanitizeText(scrubber, media, data)
		case "application/x-tar":
			return sanitizeTar(scrubber, data)
		case "application/gzip":
			return sanitizeGzipTar(scrubber, data)
		default:
			return nil, errors.New("unsupported artifact media type")
		}
	}
}

func sanitizeText(scrubber Scrubber, media string, data []byte) ([]byte, error) {
	if !safeText(data) || (media == "application/json" && !json.Valid(data)) {
		return nil, errors.New("invalid textual payload")
	}
	clean := scrubber.Scrub(data)
	if len(clean) > MaxPayloadBytes || !safeText(clean) || (media == "application/json" && !json.Valid(clean)) {
		return nil, errors.New("redaction produced invalid textual payload")
	}
	return clean, nil
}

func safeText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, b := range data {
		if b < 32 && b != '\n' && b != '\r' && b != '\t' {
			return false
		}
	}
	return true
}

const maxArchiveEntries = 256

func sanitizeTar(scrubber Scrubber, data []byte) ([]byte, error) {
	reader := tar.NewReader(bytes.NewReader(data))
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	seen := map[string]bool{}
	var total int64
	for count := 0; ; count++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("malformed reproduction archive")
		}
		if count >= maxArchiveEntries {
			return nil, errors.New("archive entry limit")
		}
		name, err := archiveName(scrubber, header.Name)
		if err != nil || seen[name] {
			return nil, errors.New("unsafe or duplicate archive name")
		}
		seen[name] = true
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return nil, errors.New("archive links and special files are unsupported")
		}
		if header.Size < 0 || header.Size > MaxPayloadBytes-total {
			return nil, errors.New("expanded archive byte limit")
		}
		total += header.Size
		cleanHeader := &tar.Header{Name: name, Typeflag: header.Typeflag, Mode: 0o644, Format: tar.FormatPAX}
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 {
				return nil, errors.New("directory carries content")
			}
			cleanHeader.Mode = 0o755
		}
		if header.Mode&0o111 != 0 {
			cleanHeader.Mode = 0o755
		}
		payload, err := io.ReadAll(io.LimitReader(reader, MaxPayloadBytes+1))
		if err != nil {
			return nil, err
		}
		media := "text/plain"
		if strings.HasSuffix(name, ".json") {
			media = "application/json"
		}
		clean, err := sanitizeText(scrubber, media, payload)
		if err != nil {
			return nil, err
		}
		cleanHeader.Size = int64(len(clean))
		if err := writer.WriteHeader(cleanHeader); err != nil {
			return nil, err
		}
		if _, err := writer.Write(clean); err != nil {
			return nil, err
		}
		if output.Len() > MaxPayloadBytes {
			return nil, errors.New("sanitized archive byte limit")
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if output.Len() > MaxPayloadBytes {
		return nil, errors.New("sanitized archive byte limit")
	}
	return output.Bytes(), nil
}

func archiveName(scrubber Scrubber, name string) (string, error) {
	name = strings.TrimSuffix(name, "/")
	if name == "" || len(name) > 512 || path.IsAbs(name) || strings.ContainsAny(name, "\\:\x00\r\n\t") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || !safeText([]byte(name)) {
		return "", errors.New("unsafe archive path")
	}
	if !bytes.Equal(scrubber.Scrub([]byte(name)), []byte(name)) {
		return "", errors.New("archive name contains secret material")
	}
	return name, nil
}

func sanitizeGzipTar(scrubber Scrubber, data []byte) ([]byte, error) {
	input := bytes.NewReader(data)
	reader, err := gzip.NewReader(input)
	if err != nil {
		return nil, errors.New("invalid gzip archive")
	}
	reader.Multistream(false)
	expanded, readErr := io.ReadAll(io.LimitReader(reader, MaxPayloadBytes+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, errors.New("invalid gzip content")
	}
	if input.Len() != 0 || len(expanded) > MaxPayloadBytes {
		return nil, errors.New("gzip trailing content or expansion limit")
	}
	clean, err := sanitizeTar(scrubber, expanded)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(clean); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("encode sanitized gzip: %w", err)
	}
	return output.Bytes(), nil
}
