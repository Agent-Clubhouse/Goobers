package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChildResolutionBodyRejectsAuthorityAndDuplicateFields(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	valid := `{"invocationKey":"inspect","action":"merge","resultRef":"` + digest + `"}`
	for _, body := range []string{valid, strings.Replace(valid, `"action":"merge"`, `"action":"merge","expectedRequestDigest":"`+digest+`"`, 1), strings.Replace(valid, `"action":"merge"`, `"action":"replace"`, 1), strings.Replace(valid, `"action":"merge"`, `"action":"discard"`, 1)} {
		if _, err := childResolutionBody(httptest.NewRequest("POST", "/", strings.NewReader(body))); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{strings.Replace(valid, `"action":"merge"`, `"action":"merge","expectedRequestDigest":"invalid"`, 1), valid + `{}`, strings.Replace(valid, `"action":"merge"`, `"action":"merge","action":"discard"`, 1), strings.Replace(valid, `"action":"merge"`, `"Action":"merge"`, 1), strings.Replace(valid, `"action":"merge"`, `"action":"commit"`, 1), strings.Replace(valid, `"action":"merge"`, `"action":"merge","parentRunId":"other"`, 1), strings.Replace(valid, digest, "result:unbound", 1), `null`, strings.Repeat(" ", 8193)} {
		if _, err := childResolutionBody(httptest.NewRequest("POST", "/", strings.NewReader(body))); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
