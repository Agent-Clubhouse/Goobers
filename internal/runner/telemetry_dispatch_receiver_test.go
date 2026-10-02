package runner

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

type dispatchProbeReceiver struct {
	mu         sync.Mutex
	keys       map[string]string
	duplicates int
	invalid    bool
}

func (p *dispatchProbeReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reader, err := gzip.NewReader(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer func() { _ = reader.Close() }()
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), 2<<20)
	p.mu.Lock()
	defer p.mu.Unlock()
	for scan.Scan() {
		var envelope struct {
			Data struct {
				BaseData struct{ Properties map[string]string }
			}
		}
		if json.Unmarshal(scan.Bytes(), &envelope) != nil {
			p.invalid = true
			continue
		}
		props := envelope.Data.BaseData.Properties
		if props["goobers.journal.kind"] != "run" {
			continue
		}
		key := props["goobers.journal.id"] + ":" + props["goobers.journal.seq"]
		id := props["goobers.telemetry.record_id"]
		if props["goobers.journal.id"] == "" || props["goobers.journal.seq"] == "" || len(id) != 64 {
			p.invalid = true
		}
		if prior, ok := p.keys[key]; ok {
			p.duplicates++
			p.invalid = p.invalid || prior != id
		}
		p.keys[key] = id
	}
	if scan.Err() != nil {
		p.invalid = true
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (p *dispatchProbeReceiver) missing(expected []string) (int, int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	missing := 0
	for _, key := range expected {
		if _, ok := p.keys[key]; !ok {
			missing++
		}
	}
	return missing, p.duplicates, p.invalid || len(p.keys) > len(expected)
}

func dispatchProbeSourceKeys(t testing.TB, runsDir string, runCount int) []string {
	t.Helper()
	var keys []string
	for i := range runCount {
		runID := fmt.Sprintf("%032x", i+1)
		file, err := os.Open(filepath.Join(runsDir, runID, "events.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		scan := bufio.NewScanner(file)
		scan.Buffer(make([]byte, 4096), 8<<20)
		sequence := 0
		var last journal.Event
		for scan.Scan() {
			sequence++
			if err := json.Unmarshal(scan.Bytes(), &last); err != nil || last.Seq != uint64(sequence) || last.Schema != journal.EventSchema {
				_ = file.Close()
				t.Fatalf("invalid source sequence %s:%d: %v", runID, sequence, err)
			}
			keys = append(keys, fmt.Sprintf("%s:%d", runID, sequence))
		}
		err = scan.Err()
		closeErr := file.Close()
		if err != nil || closeErr != nil || last.Type != journal.EventRunFinished {
			t.Fatalf("incomplete source %s: scan=%v close=%v terminal=%s", runID, err, closeErr, last.Type)
		}
	}
	return keys
}
