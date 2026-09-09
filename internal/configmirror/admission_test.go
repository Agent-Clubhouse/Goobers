package configmirror

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"testing"
)

func TestArchiveAdmissionRejectsForgedMetadata(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range []string{"instance.yaml", "config/instructions.md"} {
		if err := writeEntry(writer, name, bytes.NewBufferString("content")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	valid := buffer.Bytes()
	if err := admitArchive(bytes.NewReader(valid), int64(len(valid))); err != nil {
		t.Fatal(err)
	}
	end := len(valid) - 22
	dir := int(binary.LittleEndian.Uint32(valid[end+16 : end+20]))
	for name, mutate := range map[string]func([]byte){
		"too many entries": func(b []byte) {
			binary.LittleEndian.PutUint16(b[end+8:], MaxFiles+1)
			binary.LittleEndian.PutUint16(b[end+10:], MaxFiles+1)
		},
		"underreported entries": func(b []byte) {
			binary.LittleEndian.PutUint16(b[end+8:], 1)
			binary.LittleEndian.PutUint16(b[end+10:], 1)
		},
		"oversized name":    func(b []byte) { binary.LittleEndian.PutUint16(b[dir+28:], 4097) },
		"oversized extra":   func(b []byte) { binary.LittleEndian.PutUint16(b[dir+30:], 4097) },
		"oversized output":  func(b []byte) { binary.LittleEndian.PutUint32(b[dir+24:], MaxFileBytes+1) },
		"directory offset":  func(b []byte) { binary.LittleEndian.PutUint32(b[end+16:], uint32(len(b))) },
		"compressed method": func(b []byte) { binary.LittleEndian.PutUint16(b[dir+10:], zip.Deflate) },
		"encrypted":         func(b []byte) { binary.LittleEndian.PutUint16(b[dir+8:], 1) },
		"multidisk":         func(b []byte) { binary.LittleEndian.PutUint16(b[end+4:], 1) },
	} {
		t.Run(name, func(t *testing.T) {
			malformed := append([]byte(nil), valid...)
			mutate(malformed)
			if err := admitArchive(bytes.NewReader(malformed), int64(len(malformed))); err == nil {
				t.Fatal("accepted forged archive metadata")
			}
		})
	}
}

func TestSnapshotNameAdmissionRejectsCaseCollisionsBeforeWriting(t *testing.T) {
	seen := map[string]bool{}
	if err := reserveSnapshotName(seen, "config/Instructions.md"); err != nil {
		t.Fatal(err)
	}
	if err := reserveSnapshotName(seen, "config/instructions.md"); err == nil {
		t.Fatal("accepted a case-colliding path")
	}
	if len(seen) != 1 {
		t.Fatalf("invalid path changed admission index: %v", seen)
	}
}
