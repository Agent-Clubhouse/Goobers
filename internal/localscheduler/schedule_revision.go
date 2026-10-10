package localscheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// authoredSchedule keeps schedule identity separate from unrelated workflow edits.
type authoredSchedule struct {
	Schedule
	expression string
}

func (s authoredSchedule) scheduleIdentity() (string, error) { return s.expression, nil }
func (s locatedSchedule) scheduleIdentity() (string, error) {
	identity, err := scheduleIdentity(s.s)
	if err != nil {
		return "", err
	}
	if s.loc == nil {
		return "", errors.New("localscheduler: schedule timezone unavailable")
	}
	raw, err := json.Marshal([]string{identity, s.loc.String()})
	return string(raw), err
}
func scheduleIdentity(s Schedule) (string, error) {
	named, ok := s.(interface{ scheduleIdentity() (string, error) })
	if !ok {
		return "", errors.New("localscheduler: durable schedule lacks authored identity")
	}
	return named.scheduleIdentity()
}

// ScheduleRevision changes only when normalized schedule declarations or their
// configured timezone change. Order is retained because backoff indexes follow it.
func ScheduleRevision(schedules []Schedule) (string, error) {
	identities := make([]string, 0, len(schedules))
	for _, schedule := range schedules {
		identity, err := scheduleIdentity(schedule)
		if err != nil {
			return "", err
		}
		identities = append(identities, identity)
	}
	raw, err := json.Marshal(identities)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func normalizedScheduleExpression(expression string) string {
	return strings.Join(strings.Fields(expression), " ")
}
