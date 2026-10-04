package apireadcache

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type observedReadBody struct {
	io.Reader
	closed bool
}

func (b *observedReadBody) Close() error { b.closed = true; return nil }
func TestOversizedCacheResponseRemainsStreamingAndClosesOriginal(t *testing.T) {
	raw := bytes.Repeat([]byte("a"), apiReadCacheMaxBytes+128)
	body := &observedReadBody{Reader: bytes.NewReader(raw)}
	response := &http.Response{StatusCode: 200, Header: http.Header{"Etag": {`"large"`}}, Body: body}
	saved := false
	got, err := cacheReadResponse(response, true, func(apiReadCacheEntry) { saved = true })
	if err != nil || saved || body.closed {
		t.Fatal("large response cached/closed", err, saved, body.closed)
	}
	data, err := io.ReadAll(got.Body)
	if err != nil || !bytes.Equal(data, raw) {
		t.Fatal("stream truncated", len(data), err)
	}
	if err = got.Body.Close(); err != nil || !body.closed {
		t.Fatal("original closer lost", err)
	}
}
func TestExplicitNoStoreResponseNeverEntersSnapshot(t *testing.T) {
	for _, header := range []http.Header{{"Cache-Control": {"private, no-store"}}, {"Vary": {"*"}}} {
		body := &observedReadBody{Reader: strings.NewReader("private")}
		got, err := cacheReadResponse(&http.Response{StatusCode: 200, Header: header, Body: body}, true, func(apiReadCacheEntry) { t.Fatal("uncacheable response saved") })
		if err != nil || body.closed {
			t.Fatal(err)
		}
		if apiReadBody(t, got) != "private" || !body.closed {
			t.Fatal("response changed")
		}
	}
}
