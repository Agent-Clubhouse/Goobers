package chattest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
)

type Header struct {
	Kind     string                                        `json:"kind"`
	Format   string                                        `json:"format"`
	Version  int                                           `json:"version"`
	Protocol string                                        `json:"protocol"`
	Source   string                                        `json:"source"`
	CLI      struct{ Name, Version, VersionOutput string } `json:"cli"`
}
type Entry struct {
	Kind   string         `json:"kind"`
	From   string         `json:"from"`
	T      int            `json:"t"`
	Msg    map[string]any `json:"msg"`
	Match  map[string]any `json:"match"`
	Raw    *string        `json:"raw"`
	Stderr *string        `json:"stderr"`
	Exit   *struct {
		Code int `json:"code"`
	} `json:"exit"`
}
type Fixture struct {
	Header  Header
	Entries []Entry
}

func ReadFixture(r io.Reader) (Fixture, error) {
	var f Fixture
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 4<<20)
	line := 0
	for s.Scan() {
		line++
		if line == 1 {
			if err := json.Unmarshal(s.Bytes(), &f.Header); err != nil {
				return f, err
			}
			if f.Header.Kind != "header" || f.Header.Format != "clubhouse-chat-fixture" || f.Header.Version != 1 {
				return f, fmt.Errorf("unsupported fixture header")
			}
			continue
		}
		var e Entry
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			return f, fmt.Errorf("fixture line %d: invalid JSON", line)
		}
		if e.Kind == "note" {
			continue
		}
		if e.From != "client" && e.From != "cli" {
			return f, fmt.Errorf("fixture line %d: invalid source", line)
		}
		n := 0
		for _, yes := range []bool{e.Msg != nil, e.Raw != nil, e.Stderr != nil, e.Exit != nil} {
			if yes {
				n++
			}
		}
		if n != 1 || e.From == "client" && e.Msg == nil {
			return f, fmt.Errorf("fixture line %d: invalid payload", line)
		}
		f.Entries = append(f.Entries, e)
	}
	if line == 0 {
		return f, fmt.Errorf("empty fixture")
	}
	return f, s.Err()
}

// Replay is synchronous and deliberately does not log message bodies, including
// mismatches. receive blocks at client entries; emit receives remapped CLI data.
// The caller owns transport cancellation and timing; timestamps are metadata.
func (f Fixture) Replay(receive func() (map[string]any, error), emit func(Entry) error) error {
	ids := map[string]any{}
	for i, e := range f.Entries {
		if e.From == "client" {
			live, err := receive()
			if err != nil {
				return err
			}
			if key(live) != key(e.Msg) || !contains(live, e.Match) {
				return fmt.Errorf("fixture mismatch at entry %d", i+1)
			}
			for _, field := range []string{"id", "request_id"} {
				if id, ok := e.Msg[field]; ok {
					ids[field+fmt.Sprint(id)] = live[field]
				}
			}
			continue
		}
		// Copy recursively to keep fixtures reusable across test runs. Remap only
		// responses: server-initiated requests retain their recorded identities.
		if e.Msg != nil {
			data, _ := json.Marshal(e.Msg)
			e.Msg = nil
			_ = json.Unmarshal(data, &e.Msg)
			if e.Msg["method"] == nil && e.Msg["type"] != "control_request" {
				for _, field := range []string{"id", "request_id"} {
					if id, ok := e.Msg[field]; ok {
						if live, found := ids[field+fmt.Sprint(id)]; found {
							e.Msg[field] = live
						}
					}
				}
				if response, ok := e.Msg["response"].(map[string]any); ok {
					if live, found := ids["request_id"+fmt.Sprint(response["request_id"])]; found {
						response["request_id"] = live
					}
				}
			}
		}
		if err := emit(e); err != nil {
			return err
		}
	}
	return nil
}
func key(m map[string]any) string {
	if method, ok := m["method"].(string); ok {
		return method
	}
	if typ, ok := m["type"].(string); ok {
		if request, ok := m["request"].(map[string]any); ok {
			return typ + ":" + fmt.Sprint(request["subtype"])
		}
		return typ
	}
	return "response"
}
func contains(actual, want any) bool {
	if want == nil {
		return true
	}
	if object, ok := want.(map[string]any); ok {
		have, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range object {
			if !contains(have[k], v) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(actual, want)
}
