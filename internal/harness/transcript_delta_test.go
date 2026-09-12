package harness

import (
	"bytes"
	"sync"
	"testing"
)

func TestTranscriptDeltaCopiesLinearBytesAndKeepsCap(t *testing.T) {
	buffer := newTranscriptBuffer(1000)
	var saved []byte
	offset := 0
	for i := 0; i < 100; i++ {
		if _, err := buffer.Write(bytes.Repeat([]byte("a"), 20)); err != nil {
			t.Fatal(err)
		}
		data, next, dropped := buffer.delta(offset)
		if len(data) > 20 || next < offset || next > 1000 {
			t.Fatalf("checkpoint copied old bytes: size=%d offset=%d next=%d", len(data), offset, next)
		}
		saved = append(saved, data...)
		offset = next
		if i == 99 && dropped != 1000 {
			t.Fatalf("dropped=%d, want 1000", dropped)
		}
	}
	if len(saved) != 1000 || !bytes.Equal(buffer.Bytes(), append(saved, transcriptTruncationMarker(1000)...)) {
		t.Fatal("delta chain differs from final transcript or copied more than N bytes")
	}
	for _, invalid := range []int{-1, 1001} {
		if data, next, _ := buffer.delta(invalid); data != nil || next != invalid {
			t.Fatal("invalid cursor restarted transcript")
		}
	}
}

func TestTranscriptDeltaConcurrentCaptureAndOwnedSnapshots(t *testing.T) {
	buffer := newTranscriptBuffer(4096)
	var writes sync.WaitGroup
	writes.Go(func() {
		for i := 0; i < 1000; i++ {
			_, _ = buffer.Write([]byte("x"))
		}
	})
	offset := 0
	var saved []byte
	for i := 0; i < 100; i++ {
		data, next, _ := buffer.delta(offset)
		saved = append(saved, data...)
		clear(data) // callers own snapshots, not the live transcript backing array
		offset = next
	}
	writes.Wait()
	data, _, _ := buffer.delta(offset)
	saved = append(saved, data...)
	if !bytes.Equal(saved, bytes.Repeat([]byte("x"), 1000)) || !bytes.Equal(buffer.Bytes(), saved) {
		t.Fatal("concurrent capture lost or duplicated data, or snapshot mutated source")
	}
}
