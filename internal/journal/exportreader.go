package journal

// Incremental, read-only journal export. Never opens a writer, migrates a
// journal, or reconstructs raw JSON from the parsed envelope. Containment and
// regular-file checks apply to metadata as well as the event stream.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

// ExportPosition is a checkpoint in a journal, not an acknowledgement from a
// remote collector. Identity detects replacement; Generation detects scheduler
// compaction. Seq permits replay from offset zero after a generation change.
type ExportPosition struct {
	Identity, Generation string
	Offset               int64
	Seq                  uint64
}

// ExportBatch contains at most 128 records and approximately 256 KiB, except a
// single larger valid journal record (at most maxEventBytes). More means another
// bounded read may be needed. A torn, newline-less tail is never returned.
type ExportBatch struct {
	Events   []CommittedEvent
	Position ExportPosition
	More     bool
	Gap      bool // Retention/compaction removed a previously unconsumed sequence.
}

// ExportRunFingerprint is an incremental-read cache key, not an integrity
// signature. Include identity/schema metadata so changing either invalidates
// an EOF cache even if the event stream itself did not change. Journal writers
// publish changes with new size/mtime; restored or externally edited files must
// do the same. The rooted regular-file checks still apply on a cache hit.
func ExportRunFingerprint(ctx context.Context, root *os.Root, dir string, events os.FileInfo) (string, error) {
	buffer := make([]byte, 0, 128)
	for i, name := range []string{fileEvents, fileRunYAML, fileSchema} {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		info := events
		if i != 0 {
			var err error
			info, err = root.Stat(dir + string(os.PathSeparator) + name)
			if err != nil {
				return "", err
			}
		}
		if info == nil || !info.Mode().IsRegular() {
			return "", errors.New("journal: export metadata must be regular")
		}
		buffer = strconv.AppendInt(buffer, info.Size(), 10)
		buffer = append(buffer, ':')
		buffer = strconv.AppendInt(buffer, info.ModTime().UnixNano(), 10)
		buffer = append(buffer, ';')
	}
	return string(buffer), nil
}

func readExportMetadata(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := safeopen.OpenRegularInRoot(root, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("journal: export metadata exceeds limit")
	}
	return data, err
}

func runExportContext(root *os.Root) (CommittedEvent, string, error) {
	data, err := readExportMetadata(root, fileSchema, 4096)
	if err != nil {
		return CommittedEvent{}, "", err
	}
	var schema SchemaInfo
	if json.Unmarshal(data, &schema) != nil {
		return CommittedEvent{}, "", ErrJournalMigrationRequired
	}
	if admitted, err := admitJournalSchema("export", schema, true); err != nil || !admitted {
		return CommittedEvent{}, "", ErrJournalMigrationRequired
	}
	data, err = readExportMetadata(root, fileRunYAML, 1<<20)
	if err != nil {
		return CommittedEvent{}, "", err
	}
	var id RunIdentity
	if yaml.Unmarshal(data, &id) != nil || !id.KnownSchema() || !apiv1.ValidRunID(id.RunID) {
		return CommittedEvent{}, "", errors.New("journal: unsupported export identity")
	}
	return CommittedEvent{Kind: "run", JournalID: id.RunID, RunID: id.RunID,
		InstanceID: id.InstanceID, Gaggle: id.Gaggle, Workflow: id.Workflow,
		WorkflowVersion: id.WorkflowVersion, WorkflowDigest: id.WorkflowDigest,
		ConfigGeneration: id.ConfigGeneration, TriggerKind: string(id.Trigger.Kind)}, fileEvents, nil
}

func schedulerExportContext(root *os.Root) (CommittedEvent, string, error) {
	data, err := readExportMetadata(root, fileInstanceLogID, 128)
	if err != nil {
		return CommittedEvent{}, "", err
	}
	id := strings.TrimSuffix(string(data), "\n")
	decoded, decodeErr := hex.DecodeString(id)
	if len(data) != 33 || decodeErr != nil || len(decoded) != 16 || id != strings.ToLower(id) || id == strings.Repeat("0", 32) {
		return CommittedEvent{}, "", errors.New("journal: invalid export identity")
	}
	generation := 0
	data, err = readExportMetadata(root, fileEventsPointer, 128)
	if err == nil {
		generation, err = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	if (err != nil && !errors.Is(err, os.ErrNotExist)) || generation < 0 {
		return CommittedEvent{}, "", errors.New("journal: invalid export generation")
	}
	return CommittedEvent{Kind: "scheduler", JournalID: id}, instanceEventsFilename(generation), nil
}

// ReadExportBatch reads one standard run or scheduler directory beneath an
// already-open instance root. It only admits the currently supported schema.
func ReadExportBatch(ctx context.Context, instance *os.Root, dir string, scheduler bool, position ExportPosition, committedThrough uint64) (ExportBatch, error) {
	root, err := instance.OpenRoot(dir)
	if err != nil {
		return ExportBatch{}, err
	}
	defer func() { _ = root.Close() }()
	var base CommittedEvent
	var name string
	if scheduler {
		base, name, err = schedulerExportContext(root)
	} else {
		base, name, err = runExportContext(root)
	}
	if err != nil {
		return ExportBatch{}, err
	}
	file, err := safeopen.OpenRegularInRoot(root, name)
	if err != nil {
		return ExportBatch{}, err
	}
	defer func() { _ = file.Close() }()
	if position.Identity != base.JournalID {
		position = ExportPosition{Identity: base.JournalID}
	}
	info, err := file.Stat()
	if err != nil {
		return ExportBatch{}, err
	}
	if position.Generation != name || position.Offset > info.Size() {
		position.Offset = 0
	}
	position.Generation = name
	if _, err = file.Seek(position.Offset, io.SeekStart); err != nil {
		return ExportBatch{}, err
	}
	return scanExportBatch(ctx, file, base, position, committedThrough)
}

// Unlike bufio.ScanLines, this splitter never treats EOF as a record boundary.
func splitCommittedLine(data []byte, _ bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	return 0, nil, nil
}

func scanExportBatch(ctx context.Context, file io.Reader, base CommittedEvent, position ExportPosition, committedThrough uint64) (ExportBatch, error) {
	result := ExportBatch{Position: position}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 16<<10), maxEventBytes)
	scanner.Split(splitCommittedLine)
	readBytes := 0
	for scanned := 0; scanned < 128 && readBytes < 256<<10; scanned++ {
		if err := ctx.Err(); err != nil {
			return ExportBatch{}, err
		}
		if !scanner.Scan() {
			return result, scanner.Err()
		}
		line := scanner.Bytes()
		readBytes += len(line)
		result.Position.Offset += int64(len(line))
		body := bytes.TrimSpace(bytes.TrimLeft(line, "\x00"))
		if len(body) == 0 {
			continue
		}
		var event Event
		if json.Unmarshal(body, &event) != nil || !event.KnownSchema() || event.Seq == 0 {
			return ExportBatch{}, fmt.Errorf("journal: unsupported or corrupt export record")
		}
		if committedThrough > 0 && event.Seq > committedThrough {
			result.Position.Offset -= int64(len(line))
			return result, nil
		}
		if event.Seq <= result.Position.Seq {
			continue // A compacted scheduler generation retains some older events.
		}
		if result.Position.Seq > 0 && event.Seq != result.Position.Seq+1 {
			result.Gap = true
		}
		result.Position.Seq = event.Seq
		committed := base
		committed.Seq, committed.Time, committed.ObservedTime = event.Seq, event.Time, time.Now()
		committed.Stage, committed.Attempt = event.Stage, event.Attempt
		committed.Body = bytes.Clone(body)
		if event.Gaggle != "" {
			committed.Gaggle = event.Gaggle
		}
		if event.Workflow != "" {
			committed.Workflow = event.Workflow
		}
		if event.RunID != "" {
			committed.RunID = event.RunID
		}
		result.Events = append(result.Events, committed)
		if committedThrough > 0 && event.Seq == committedThrough {
			return result, nil
		}
	}
	result.More = true
	return result, nil
}
