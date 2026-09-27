package panicguard

import "testing"

func TestCallReportsPanicAndReturnsNormally(t *testing.T) {
	got := 0
	if panicked := Call(func(v int) { got = v; panic("boom") }, 7); !panicked {
		t.Fatal("panicked = false; want true")
	}
	if got != 7 {
		t.Fatalf("fn saw %d; want the argument 7", got)
	}
}

func TestCallReportsNoPanic(t *testing.T) {
	got := 0
	if panicked := Call(func(v int) { got = v }, 3); panicked {
		t.Fatal("panicked = true; want false")
	}
	if got != 3 {
		t.Fatalf("fn saw %d; want 3", got)
	}
}

func TestCallContainsNilPanicValue(t *testing.T) {
	// A panic with a nil value still unwinds, and Go turns it into a
	// *runtime.PanicNilError, so recover observes a non-nil value.
	if panicked := Call(func(struct{}) { panic(nil) }, struct{}{}); !panicked {
		t.Fatal("panicked = false for panic(nil); want true")
	}
}
