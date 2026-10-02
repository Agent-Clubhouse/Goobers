//go:build unix

package launchreceipt

import (
	"context"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAttestationReaderRejectsFIFOWithoutBlocking(t *testing.T) {
	receipt := receiptFixture()
	reader, path := fixtureReader(t, ControlPlaneStore, receipt)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Assemble(context.Background(), receipt.Binding, MaxBytes); err == nil {
		t.Fatal("FIFO accepted")
	}
}
