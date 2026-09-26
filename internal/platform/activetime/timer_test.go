package activetime

import (
	"testing"
	"time"
)

func TestTimerFiresAfterActiveDuration(t *testing.T) {
	timer := NewTimer(25 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-time.After(2 * time.Second):
		t.Fatal("active-time timer did not fire")
	}
}

func TestTimerStopPreventsFiring(t *testing.T) {
	timer := NewTimer(time.Second)
	if !timer.Stop() {
		t.Fatal("first Stop returned false")
	}
	if timer.Stop() {
		t.Fatal("second Stop returned true")
	}

	select {
	case <-timer.C:
		t.Fatal("stopped active-time timer fired")
	case <-time.After(50 * time.Millisecond):
	}
}
