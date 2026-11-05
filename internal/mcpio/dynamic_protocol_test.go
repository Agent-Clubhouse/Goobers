package mcpio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDynamicProtocolIgnoresNotificationsAndRejectsInvalidRequests(t *testing.T) {
	calls := 0
	server := NewDynamicServer("synthetic", nil, func(string, json.RawMessage) (any, error) {
		calls++
		return DynamicResult{Value: map[string]bool{"success": true}}, nil
	})
	var stdout, stderr bytes.Buffer
	if err := server.Serve(strings.NewReader(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"synthetic","arguments":{}}}
[]
{"jsonrpc":"1.0","id":1,"method":"ping"}
{"jsonrpc":"2.0","id":2,"method":"ping"}
`), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("notification executed tool", calls)
	}
	if strings.Count(stdout.String(), "-32600") != 2 || !strings.Contains(stdout.String(), `"result":{}`) {
		t.Fatal(stdout.String())
	}
}
