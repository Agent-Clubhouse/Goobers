package artifactset

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/google/pprof/profile"
	"google.golang.org/protobuf/encoding/protowire"
)

func sanitizePprof(scrubber Scrubber, data []byte) ([]byte, error) {
	input := bytes.NewReader(data)
	z, err := gzip.NewReader(input)
	if err != nil {
		return nil, errors.New("invalid compressed profile")
	}
	z.Multistream(false)
	raw, readErr := io.ReadAll(io.LimitReader(z, MaxPayloadBytes+1))
	if err := errors.Join(readErr, z.Close()); err != nil || len(raw) > MaxPayloadBytes || input.Len() != 0 {
		return nil, errors.New("invalid profile compression or expansion limit")
	}
	// Count fields and packed scalar elements before the profile parser builds
	// object graphs. Bytes alone do not bound a compact protobuf's allocations.
	budget := 1 << 18
	if err := checkProfileWire(raw, "profile", &budget); err != nil {
		return nil, err
	}
	p, err := profile.ParseUncompressed(raw)
	if err != nil || p.CheckValid() != nil || len(p.SampleType) == 0 {
		return nil, errors.New("invalid native profile")
	}
	// All strings in this closed protobuf schema are uncompressed string-table
	// entries. Reject unsafe evidence rather than corrupting indexed strings.
	if !bytes.Equal(raw, scrubber.Scrub(raw)) {
		return nil, errors.New("native profile requires unsafe binary redaction")
	}
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(raw); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if output.Len() > MaxPayloadBytes {
		return nil, errors.New("compressed profile byte limit")
	}
	return output.Bytes(), nil
}

// Empty kind means scalar varint; "packed" also admits packed varints;
// "string" means a UTF-8 string. Other kinds name nested messages. Reject
// unknown fields so opaque future payloads cannot bypass the redaction policy.
var profileWire = map[string][]string{
	"profile":  {"", "value", "sample", "mapping", "location", "function", "string", "", "", "", "", "value", "", "packed", "", ""},
	"value":    {"", "", ""},
	"sample":   {"", "packed", "packed", "label"},
	"mapping":  {"", "", "", "", "", "", "", "", "", "", ""},
	"location": {"", "", "", "", "line", ""},
	"function": {"", "", "", "", "", ""},
	"label":    {"", "", "", "", ""},
	"line":     {"", "", "", ""},
}

func checkProfileWire(data []byte, message string, budget *int) error {
	fields := profileWire[message]
	for len(data) != 0 {
		*budget--
		number, wire, n := protowire.ConsumeTag(data)
		if *budget < 0 || n < 0 || number <= 0 || int(number) >= len(fields) {
			return errors.New("invalid profile field or element limit")
		}
		data = data[n:]
		kind := fields[number]
		if wire == protowire.VarintType && (kind == "" || kind == "packed") {
			_, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return errors.New("invalid profile scalar")
			}
			data = data[n:]
			continue
		}
		if wire != protowire.BytesType || kind == "" {
			return errors.New("invalid profile wire type")
		}
		payload, n := protowire.ConsumeBytes(data)
		if n < 0 {
			return errors.New("truncated profile field")
		}
		data = data[n:]
		if err := checkProfilePayload(payload, kind, budget); err != nil {
			return err
		}
	}
	return nil
}

func checkProfilePayload(data []byte, kind string, budget *int) error {
	switch kind {
	case "string":
		if !utf8.Valid(data) {
			return errors.New("invalid profile string")
		}
	case "packed":
		for len(data) != 0 {
			*budget--
			_, n := protowire.ConsumeVarint(data)
			if *budget < 0 || n < 0 {
				return errors.New("invalid packed profile field or element limit")
			}
			data = data[n:]
		}
	default:
		return checkProfileWire(data, kind, budget)
	}
	return nil
}
