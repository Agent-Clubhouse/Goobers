package podauth

import (
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
)

func TestChildWorkflowGrantPrefixMatchesPodauth(t *testing.T) {
	if ChildWorkflowGrantPrefix != httpapi.ChildWorkflowGrantTokenPrefix {
		t.Fatal("child workflow grant prefix drift")
	}
}
