package parallelworkspace

import (
	"errors"
	"reflect"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
)

func validateJoinPreparation(reader *journal.Reader, value Preparation) error {
	states, err := spec.ReadJoins(reader)
	if err != nil {
		return err
	}
	request := value.Request
	state := states[request.Sequence]
	if state == nil || request.Branch != 0 || request.Status != "" {
		return errors.New("parallel merge capture has no exact root intent")
	}
	intent, err := readJoinIntent(reader, *state)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(intent.Request.Request, request.Request) || intent.Request.Plan != request.Plan || intent.Request.Seed != request.Seed || intent.Request.Root != request.Custody || !reflect.DeepEqual(joinSnapshot(intent.Prepared), value.Snapshot) {
		return errors.New("parallel merge capture changed durable preparation")
	}
	return nil
}

// AcknowledgedJoinPreparation permits normal snapshot cleanup only after its
// exact application has completed. The caller must additionally verify archival
// of the root and every branch; unacknowledged intent remains recoverable.
func AcknowledgedJoinPreparation(reader *journal.Reader, value Preparation) error {
	if !value.Request.Join {
		return errors.New("capture is not a parallel merge preparation")
	}
	if err := validateJoinPreparation(reader, value); err != nil {
		return err
	}
	states, err := spec.ReadJoins(reader)
	if err != nil {
		return err
	}
	state := states[value.Request.Sequence]
	if state == nil || !state.Applied {
		return errors.New("parallel merge application remains unfinished")
	}
	intent, err := readJoinIntent(reader, *state)
	if err != nil {
		return err
	}
	_, _, err = readJoinReady(reader, *state, intent)
	return err
}
