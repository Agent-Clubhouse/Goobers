package configmirror

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"testing"
	"testing/fstest"
	"time"
)

func TestWriteSnapshotClosesSourceFileBeforeNextEntry(t *testing.T) {
	const fileCount = 512
	files := make(fstest.MapFS, fileCount)
	for i := range fileCount {
		name := fmt.Sprintf("file-%04d.txt", i)
		files[name] = &fstest.MapFile{
			Data: []byte(fmt.Sprintf("payload-%04d", i)),
			Mode: 0o640,
		}
	}
	openFiles := 0
	maxOpenFiles := 0
	var out bytes.Buffer
	err := writeSnapshotFromFS(t.Context(), &out, files, func(name string) (snapshotSourceFile, error) {
		entry, ok := files[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		openFiles++
		maxOpenFiles = max(maxOpenFiles, openFiles)
		if openFiles > 1 {
			return nil, fmt.Errorf("opened %d source files concurrently", openFiles)
		}
		return &trackingSourceFile{
			Reader: bytes.NewReader(entry.Data),
			info: staticFileInfo{
				name: path.Base(name),
				size: int64(len(entry.Data)),
				mode: entry.Mode,
			},
			closeFunc: func() error {
				openFiles--
				return nil
			},
		}, nil
	}, []byte("generation"))
	if err != nil {
		t.Fatal(err)
	}
	if openFiles != 0 || maxOpenFiles != 1 {
		t.Fatalf("open source files after snapshot = %d, max = %d", openFiles, maxOpenFiles)
	}
	reader, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	wantEntries := fileCount + 2
	if len(reader.File) != wantEntries {
		t.Fatalf("archive entries = %d, want %d", len(reader.File), wantEntries)
	}
	for _, name := range []string{"instance.yaml", "config/file-0000.txt", "config/file-0511.txt"} {
		if got := readZipEntry(t, reader, name); got == "" {
			t.Fatalf("archive entry %q is empty or missing", name)
		}
	}
}

func TestWriteSnapshotReturnsSourceFileCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	files := fstest.MapFS{
		"instructions.md": &fstest.MapFile{Data: []byte("payload"), Mode: 0o640},
	}
	var out bytes.Buffer
	err := writeSnapshotFromFS(t.Context(), &out, files, func(name string) (snapshotSourceFile, error) {
		entry, ok := files[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return &trackingSourceFile{
			Reader: bytes.NewReader(entry.Data),
			info: staticFileInfo{
				name: name,
				size: int64(len(entry.Data)),
				mode: entry.Mode,
			},
			closeFunc: func() error { return closeErr },
		}, nil
	}, []byte("generation"))
	if !errors.Is(err, closeErr) {
		t.Fatalf("writeSnapshotFromFS error = %v, want close error", err)
	}
}

func readZipEntry(t *testing.T, reader *zip.Reader, name string) string {
	t.Helper()
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		body, err := readZipFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	t.Fatalf("missing zip entry %q", name)
	return ""
}

func readZipFile(file *zip.File) (_ []byte, retErr error) {
	rc, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rc.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	var out bytes.Buffer
	_, err = out.ReadFrom(rc)
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type trackingSourceFile struct {
	*bytes.Reader
	info      fs.FileInfo
	closeFunc func() error
}

func (f *trackingSourceFile) Stat() (fs.FileInfo, error) {
	return f.info, nil
}

func (f *trackingSourceFile) Close() error {
	return f.closeFunc()
}

type staticFileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (i staticFileInfo) Name() string       { return i.name }
func (i staticFileInfo) Size() int64        { return i.size }
func (i staticFileInfo) Mode() fs.FileMode  { return i.mode }
func (i staticFileInfo) ModTime() time.Time { return time.Time{} }
func (i staticFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i staticFileInfo) Sys() any           { return nil }
