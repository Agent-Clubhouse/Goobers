package recovery

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type preparedPublicationLog struct{ calls int }

func (l *preparedPublicationLog) Append(journal.Event) error {
	l.calls++
	return nil
}

func TestRetainPreparedRejectsChangedOwnershipBeforePublication(t *testing.T) {
	prepared := storageTestRecord()
	prepared.ArchiveDigest, prepared.ArchiveBytes, prepared.ArchiveFormat = "", 0, ""
	for _, field := range []string{"repository", "run", "created", "deadline", "archive", "journal"} {
		t.Run(field, func(t *testing.T) {
			record := prepared
			request := RetentionRequest{RepositoryKey: record.RepositoryKey, RunID: record.RunID, IdentityTime: record.CreatedAt, RetainUntil: record.RetainUntil}
			log := &preparedPublicationLog{}
			var publication PublicationJournal = log
			switch field {
			case "repository":
				request.RepositoryKey = "changed"
			case "run":
				request.RunID = "changed"
			case "created":
				request.IdentityTime = request.IdentityTime.Add(time.Second)
			case "deadline":
				request.RetainUntil = request.RetainUntil.Add(time.Second)
			case "archive":
				record.ArchiveBytes = 1
			case "journal":
				publication = nil
			}
			got, path, err := RetainPrepared(t.Context(), request, record, publication)
			if err == nil || got != (Record{}) || path != "" || log.calls != 0 {
				t.Fatal("changed preparation accepted", got, path, err)
			}
		})
	}
}
