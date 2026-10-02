package dispatcher

import "encoding/json"

func artifactPublicationEnv(attempt Attempt) string {
	if attempt.Agentic || attempt.ArtifactPublication == nil {
		return ""
	}
	// The contract contains only strings, integers, and slices.
	data, _ := json.Marshal(attempt.ArtifactPublication)
	return string(data)
}
