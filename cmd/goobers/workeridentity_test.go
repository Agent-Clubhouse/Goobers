package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func TestWorkerRootGuardPrecedesRuntimeStorage(t *testing.T) {
	for _, source := range []string{"flag", "environment"} {
		for _, historical := range []bool{false, true} {
			t.Run(source+"/historical="+strconv.FormatBool(historical), func(t *testing.T) {
				root := initScheduledDemo(t)
				if historical {
					if _, err := instance.DecommissionRoot(context.Background(), root, "migrated", time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				workRoot := filepath.Join(t.TempDir(), "work")
				blobRoot := filepath.Join(t.TempDir(), "blobs")
				args := []string{"--work-root", workRoot, "--blob-store", blobRoot}
				if source == "flag" {
					args = append(args, "--instance", root)
				} else {
					t.Setenv("GOOBERS_INSTANCE_ROOT", root)
				}
				var stdout, diagnostic bytes.Buffer
				var stderr io.Writer = &diagnostic
				if !historical {
					stderr = &workerIdentityRejectingWriter{t: t, root: root, workRoot: workRoot, blobRoot: blobRoot}
				}
				if code := runWorker(args, &stdout, stderr); code != 2 {
					t.Fatalf("worker bypassed root guard: exit %d, output %s, diagnostic %s", code, stdout.String(), diagnostic.String())
				}
				if historical && !strings.Contains(diagnostic.String(), "historical root; do not use") {
					t.Fatalf("historical refusal missing: %s", diagnostic.String())
				}
				for _, path := range []string{workRoot, blobRoot} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("worker allocated storage before root guard: %s: %v", path, err)
					}
				}
			})
		}
	}
}

type workerIdentityRejectingWriter struct {
	t                        *testing.T
	root, workRoot, blobRoot string
	seen                     bool
}

func (w *workerIdentityRejectingWriter) Write(data []byte) (int, error) {
	w.t.Helper()
	if !w.seen {
		w.seen = true
		if string(data) != manualServiceRootHeader(w.t, w.root) {
			w.t.Fatalf("worker's first diagnostic did not identify its root: %s", data)
		}
		for _, path := range []string{w.workRoot, w.blobRoot} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				w.t.Fatalf("storage existed before banner: %s: %v", path, err)
			}
		}
	}
	return 0, io.ErrClosedPipe
}
