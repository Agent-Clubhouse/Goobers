package sharedclaim

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCoordinationEncodingSeparatesSharedAndLocalRecords(t *testing.T) {
	key := "github/acme/repo/issues/42"
	lease := Record{Version: 1, Owner: Owner{"instance", "run", "token"}, ExpiresAt: time.Now().UTC()}
	encoded, err := Encode(key, lease)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(key, encoded)
	if err != nil || decoded != lease {
		t.Fatalf("shared record round trip: %+v %v", decoded, err)
	}
	for _, data := range [][]byte{
		[]byte(`{"mode":"local","run":"run"}`),
		bytes.Replace(encoded, []byte(coordinationProtocol), []byte("goobers/local-mirror/v1"), 1),
		bytes.Replace(encoded, []byte("issues/42"), []byte("issues/43"), 1),
		append(append([]byte{}, encoded...), []byte(` {}`)...),
		[]byte(strings.Repeat(" ", MaxRecordBytes+1)),
		bytes.Replace(encoded, []byte(`"protocol":`), []byte(`"unknown":true,"protocol":`), 1),
		bytes.Replace(encoded, []byte(`"protocol":`), []byte(`"protocol":"local","protocol":`), 1),
	} {
		if _, err := Decode(key, data); err == nil {
			t.Fatal("non-shared or malformed record admitted")
		}
	}
}

func TestReleasedCoordinationRecordRoundTrip(t *testing.T) {
	data, err := Encode("issue:42", Record{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode("issue:42", data)
	if err != nil || got != (Record{Version: 1}) {
		t.Fatalf("tombstone round trip: %+v %v", got, err)
	}
}
