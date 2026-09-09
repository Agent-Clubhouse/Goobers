package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/goobers/goobers/internal/dispatcher"
)

func podSurrenderAttempt() (int, error) {
	raw := os.Getenv(dispatcher.EnvPodAttempt)
	if raw == "" {
		raw = os.Getenv(dispatcher.EnvAttempt)
	}
	attempt, err := strconv.Atoi(raw)
	if err != nil || attempt < 1 {
		return 0, fmt.Errorf("invalid pod surrender attempt %q", raw)
	}
	return attempt, nil
}

// New pods scope idempotency to their physical dispatch, so a later visit's
// artifact/telemetry cannot be mistaken for a redelivery from an earlier pod.
// Unstamped legacy pods keep their original keys.
func podJournalOpKey(key string) string {
	return podJournalKey(os.Getenv(dispatcher.EnvPodAttempt), key)
}

func podJournalKey(physicalAttempt, key string) string {
	if physicalAttempt == "" {
		return key
	}
	return "pod/" + physicalAttempt + "/" + key
}
