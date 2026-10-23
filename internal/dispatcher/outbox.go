package dispatcher

import "encoding/json"

func stageOutboxEnv(attempt Attempt) string {
	if len(attempt.Outbox) == 0 {
		return ""
	}
	data, _ := json.Marshal(attempt.Outbox)
	return string(data)
}
