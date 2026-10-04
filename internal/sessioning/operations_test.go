package sessioning

import "testing"

func TestSessionOperationArgumentsHaveClosedAuthorityFreeShape(t *testing.T) {
	valid := `{"sourceBindingId":"planning","id":"42","expectedSourceId":"I_node"}`
	if _, err := DecodeBacklogRead([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"sourceBindingId":"planning","id":"1","id":"2"}`, `{"SourceBindingId":"planning","id":"1"}`, `{"sourceBindingId":"planning","id":"1","actor":"admin"}`, `{"sourceBindingId":"planning","id":null}`, `{"sourceBindingId":"planning","id":"1"}{}`, `{"sourceBindingId":"../other","id":"1"}`} {
		if _, err := DecodeBacklogRead([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"sourceBindingId":"planning","limit":101}`, `{"sourceBindingId":"planning","limit":-1}`, `{"sourceBindingId":"planning","cursor":null}`, `{"sourceBindingId":"planning","limit":1,"limit":2}`} {
		if _, err := DecodeBacklogList([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
