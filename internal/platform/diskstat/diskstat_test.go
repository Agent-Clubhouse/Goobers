package diskstat

import "testing"

func TestReadCurrentDirectory(t *testing.T) {
	footprint, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if footprint.TotalBytes == 0 {
		t.Fatalf("expected non-zero TotalBytes, got %+v", footprint)
	}
	if footprint.AvailableBytes > footprint.TotalBytes {
		t.Fatalf("AvailableBytes %d exceeds TotalBytes %d", footprint.AvailableBytes, footprint.TotalBytes)
	}
}

func TestReadMissingPath(t *testing.T) {
	if _, err := Read("/definitely/does/not/exist/goobers-diskstat-test"); err == nil {
		t.Fatal("expected error for missing path")
	}
}
