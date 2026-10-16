package main

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
)

// TestBuiltinResultFilesAvoidReservedGateInputKeys is the contract behind
// Goobers#7052: gate.AutomatedInputs rejects any gate whose subject emits
// "status", so a builtin whose result file carries a top-level "status" fails
// every automated gate downstream of it. errorCode/errorMessage/errorRetryable
// are the executor's typed failure channel (shell.go consumes and deletes them
// from outputs), so only the failure-only receipt fields may carry them.
func TestBuiltinResultFilesAvoidReservedGateInputKeys(t *testing.T) {
	failureChannel := map[string]bool{
		gate.InputKeyErrorCode: true, gate.InputKeyErrorMessage: true, gate.InputKeyErrorRetryable: true,
	}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[apiv1.ReviewThreadPublication](),
		reflect.TypeFor[cancelPendingCIResult](),
	} {
		for i := 0; i < typ.NumField(); i++ {
			name, opts, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if name == gate.InputKeyStatus {
				t.Errorf("%s field %s emits reserved gate input key %q", typ.Name(), typ.Field(i).Name, name)
			}
			if failureChannel[name] && !strings.Contains(opts, "omitempty") {
				t.Errorf("%s field %s emits %q unconditionally; it is the failure-only channel", typ.Name(), typ.Field(i).Name, name)
			}
		}
	}
}
