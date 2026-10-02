package k8spreflight

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// ValidateRecordingFlags prevents silent misuse of the opt-in recording flags.
func ValidateRecordingFlags(fs *flag.FlagSet, k8s bool, root string, maxAge time.Duration) error {
	var requested bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "record-instance" || f.Name == "result-max-age" {
			requested = true
		}
	})
	if !requested {
		return nil
	}
	if !k8s || root == "" || maxAge <= 0 {
		return fmt.Errorf("--record-instance and a positive --result-max-age require --k8s and a nonempty instance root")
	}
	return nil
}

// RecordUnavailable replaces each selected check with a failed observation when
// client initialization fails. It stores no raw error, endpoint or credentials.
func RecordUnavailable(root string, selected []string, maxAge time.Duration) error {
	report := Report{}
	for _, check := range checkDefinitions() {
		if len(selected) > 0 && !slices.Contains(selected, check.id) {
			continue
		}
		report.Results = append(report.Results, Result{ID: check.id, Status: StatusFail})
	}
	return RecordResults(root, report, maxAge)
}

// RecordResults appends credential-free results to the instance journal, using
// its normal cross-process locking and durability. Only the explicit recording
// root enables this side effect; install-time doctor invocations stay read-only.
func RecordResults(root string, report Report, maxAge time.Duration) error {
	if root == "" {
		return nil
	}
	if maxAge <= 0 {
		return fmt.Errorf("result max age must be positive")
	}
	now := time.Now().UTC()
	log, _, err := journal.OpenInstanceLog(instance.NewLayout(root).SchedulerDir(), journal.WithClock(func() time.Time { return now }))
	if err != nil {
		return err
	}
	for _, result := range report.Results {
		err = log.Append(journal.Event{Type: journal.EventClusterCheckCompleted, Runner: map[string]any{
			"check": result.ID, "outcome": string(result.Status), "expiresAt": now.Add(maxAge).Format(time.RFC3339Nano),
		}})
		if err != nil {
			break
		}
	}
	return errors.Join(err, log.Close())
}
