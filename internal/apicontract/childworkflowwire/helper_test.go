package childworkflowwire

import (
	"strings"
	"testing"
)

func TestHelperRequestByteBounds(t *testing.T) {
	request := HelperRequest{Task: strings.Repeat("é", MaxHelperTaskBytes/2), Goober: "coder"}
	if err := request.Validate(); err != nil {
		t.Fatalf("exact byte limit refused: %v", err)
	}
	request.Task += "é"
	if err := request.Validate(); err == nil {
		t.Fatal("rune count bypassed task byte limit")
	}
	for _, task := range []string{"", " \n\t", "x\x00y", "x\x1by", string([]byte{0xff})} {
		if err := (HelperRequest{Task: task, Goober: "coder"}).Validate(); err == nil {
			t.Fatalf("invalid task accepted: %q", task)
		}
	}
	for _, request := range []HelperRequest{
		{Task: "Inspect", Goober: " "},
		{Task: "Inspect", Goober: strings.Repeat("x", 129)},
		{Task: "Inspect", Goober: "coder", Harness: "model --bypass"},
		{Task: "Inspect", Goober: "coder", Model: strings.Repeat("x", 257)},
		{Task: "Inspect", Goober: "coder", Base: "parent\nother"},
	} {
		if err := request.Validate(); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
}
