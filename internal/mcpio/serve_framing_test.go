package mcpio

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

type repeatedFrameByte byte

func (b repeatedFrameByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func TestServeRecoversFromMalformedAndOversizedFrames(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  io.Reader
	}{
		{"malformed", strings.NewReader("{broken\n")},
		{"oversized", io.MultiReader(io.LimitReader(repeatedFrameByte('x'), maxFrameBytes+1), strings.NewReader("\n"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newTestServer(t)
			valid := rpcLine(t, 7, "tools/list", nil)
			notification := rpcLine(t, -1, "notifications/initialized", nil)
			input := io.MultiReader(tc.bad, strings.NewReader(notification+valid))
			var output, stderr bytes.Buffer
			if err := server.Serve(input, &output, &stderr); err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(&output)
			var bad, good rpcResponse
			if err := decoder.Decode(&bad); err != nil {
				t.Fatal(err)
			}
			if bad.JSONRPC != "2.0" || string(bad.ID) != "null" || bad.Error == nil || bad.Error.Code != -32700 {
				t.Fatalf("parse response = %+v", bad)
			}
			if err := decoder.Decode(&good); err != nil {
				t.Fatal(err)
			}
			if string(good.ID) != "7" || good.Error != nil {
				t.Fatalf("later response = %+v", good)
			}
			if err := decoder.Decode(&good); !errors.Is(err, io.EOF) {
				t.Fatalf("extra response (including notification): %v", err)
			}
		})
	}
}

func TestReadFrameBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		oversized         bool
		err               error
	}{
		{"empty eof", "", "", false, io.EOF},
		{"empty line", "\n", "", false, nil},
		{"limit", strings.Repeat("x", 32) + "\n", strings.Repeat("x", 32), false, nil},
		{"limit eof", strings.Repeat("x", 32), strings.Repeat("x", 32), false, io.EOF},
		{"excess", strings.Repeat("x", 33) + "\n", "", true, nil},
		{"excess eof", strings.Repeat("x", 33), "", true, io.EOF},
		{"crlf", "hello\r\n", "hello\r", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := bufio.NewReaderSize(strings.NewReader(tc.input), 16)
			line, oversized, err := readFrame(reader, 32)
			if string(line) != tc.want || oversized != tc.oversized || !errors.Is(err, tc.err) {
				t.Fatalf("readFrame = %q, %v, %v", line, oversized, err)
			}
			if cap(line) > 32 {
				t.Fatalf("retained capacity = %d, exceeds limit", cap(line))
			}
		})
	}
}

func TestServeFinalFrameAndIOErrors(t *testing.T) {
	server, _ := newTestServer(t)
	request := strings.TrimSuffix(rpcLine(t, 1, "tools/list", nil), "\n")
	var output bytes.Buffer
	if err := server.Serve(strings.NewReader(request), &output, io.Discard); err != nil || output.Len() == 0 {
		t.Fatalf("unterminated final frame: response bytes=%d, err=%v", output.Len(), err)
	}
	output.Reset()
	if err := server.Serve(io.LimitReader(repeatedFrameByte('x'), maxFrameBytes+1), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var oversized rpcResponse
	if err := json.Unmarshal(output.Bytes(), &oversized); err != nil || oversized.Error == nil || oversized.Error.Code != -32700 {
		t.Fatalf("oversized final frame: %+v, %v", oversized, err)
	}
	failure := errors.New("broken stream")
	if err := server.Serve(io.MultiReader(strings.NewReader("{partial"), failingFrameIO{failure}), io.Discard, io.Discard); !errors.Is(err, failure) {
		t.Fatalf("read error = %v", err)
	}
	for _, input := range []string{request, "{broken"} {
		if err := server.Serve(strings.NewReader(input), failingFrameIO{failure}, io.Discard); !errors.Is(err, failure) {
			t.Fatalf("write error = %v", err)
		}
	}
}

type failingFrameIO struct{ err error }

func (f failingFrameIO) Read([]byte) (int, error)  { return 0, f.err }
func (f failingFrameIO) Write([]byte) (int, error) { return 0, f.err }
