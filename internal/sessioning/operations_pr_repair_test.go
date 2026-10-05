package sessioning

import (
	"strings"
	"testing"
)

func TestPRRepairToolDecodeClosesNestedAuthorityAndBoundsIntent(t *testing.T) {
	base := `{"requestId":"one","expectedHeadSha":"` + strings.Repeat("a", 40) + `","rationale":"Fix selected PR","changes":[{"path":"file.txt","content":"new"}]}`
	if _, err := DecodePRRepairRequest([]byte(base)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(base, `"requestId":"one"`, `"requestId":"one","requestId":"two"`, 1),
		strings.Replace(base, `"requestId":"one"`, `"requestId":"one","actor":"admin"`, 1),
		strings.Replace(base, `"content":"new"`, `"content":"new","Content":"different"`, 1),
		strings.Replace(base, `"content":"new"`, `"content":"new","credential":"secret"`, 1),
		strings.Replace(base, `"content":"new"`, `"content":null`, 1),
		strings.Replace(base, `"file.txt"`, `"../file.txt"`, 1),
		strings.Replace(base, `"file.txt"`, `".git/config"`, 1),
		strings.Replace(base, `"file.txt"`, `".goobers/key"`, 1),
		strings.Replace(base, `"new"`, `"`+strings.Repeat("x", 1<<20+1)+`"`, 1),
	} {
		if _, err := DecodePRRepairRequest([]byte(raw)); err == nil {
			t.Fatal("unsafe repair intent accepted")
		}
	}
	for _, raw := range []string{`{"repository":"foreign"}`, `{"parentCommandId":"workbench-` + strings.Repeat("a", 32) + `"}`} {
		if _, err := DecodePRRepairInspect([]byte(raw)); err == nil {
			t.Fatal("inspection authority injection")
		}
	}
	if _, err := DecodePRRepairRead([]byte(`{"path":"file.txt","runId":"foreign"}`)); err == nil {
		t.Fatal("read run injection")
	}
	if _, err := DecodePRRepairReceipt([]byte(`{"commandId":"repair-` + strings.Repeat("a", 32) + `","actor":"foreign"}`)); err == nil {
		t.Fatal("receipt actor injection")
	}
}
