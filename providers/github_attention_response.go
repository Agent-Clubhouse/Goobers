package providers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// Only an absent Link field certifies a complete explicitly requested first
// page. Any pagination evidence, including an empty or unrecognized Link, is
// conservatively partial; no provider-supplied URL is followed here.
func attentionFirstPageComplete(header http.Header) bool {
	return len(header.Values("Link")) == 0
}

// readAttentionJSON requires one complete bounded JSON value of the expected
// shape. A 204, null, or valid value followed by more data is not empty evidence.
func readAttentionJSON(response *http.Response, endpoint string, shape byte, out any) error {
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return newProviderResponseError(response, http.MethodGet, endpoint, body)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxAttentionResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > MaxAttentionResponseBytes {
		return ErrAttentionChanged
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != shape || json.Unmarshal(body, out) != nil {
		return ErrAttentionChanged
	}
	return nil
}

func readAttentionArray[T any](response *http.Response, endpoint string) ([]T, error) {
	var raw []json.RawMessage
	if err := readAttentionJSON(response, endpoint, '[', &raw); err != nil {
		return nil, err
	}
	result := make([]T, 0, len(raw))
	for _, entry := range raw {
		var value T
		if len(entry) == 0 || entry[0] != '{' || json.Unmarshal(entry, &value) != nil {
			return nil, ErrAttentionChanged
		}
		result = append(result, value)
	}
	return result, nil
}
