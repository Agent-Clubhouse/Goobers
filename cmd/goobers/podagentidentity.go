package main

import (
	"fmt"
	"os"
	"sync/atomic"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

// All recorder values in a stage pod share one append sequence. Adapters can
// emit distinct events with identical payloads and no source sequence. Allocate
// identity once before HTTPEmitter retries the constructed operation; retries
// retain the key, while another Append always represents another observation.
// Stage pods use RestartPolicyNever, so a new process gets a new pod attempt.
var podAgentAppendSequence atomic.Uint64

func podAgentEventOpKey(event journal.Event) string {
	key := fmt.Sprintf("%s/%s/%d", os.Getenv(dispatcher.EnvStage), event.Type, event.Seq)
	if os.Getenv(dispatcher.EnvPodAttempt) == "" {
		return key // Preserve unstamped legacy pods' idempotency contract.
	}
	return podJournalOpKey(fmt.Sprintf("%s/append/%d", key, podAgentAppendSequence.Add(1)))
}
