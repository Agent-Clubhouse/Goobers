package dispatcher

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/capability"
)

func agenticMergeAuthority(attempt Attempt) bool {
	return attempt.Agentic && (slices.Contains(attempt.Capabilities, string(capability.GitHubPRMerge)) || slices.Contains(attempt.Capabilities, string(capability.ADOPRComplete)))
}

func (d *Dispatcher) mintAgenticMergeJournal(attempt *Attempt) error {
	if d.cfg.WriteAPIBase == "" || d.cfg.TokenMinter == nil {
		return nil
	}
	minter, ok := d.cfg.TokenMinter.(ScopedTokenMinter)
	if !ok {
		return fmt.Errorf("agentic merge requires a scoped journal token minter")
	}
	token, err := minter.MintScoped(attempt.RunID, PlaneTokenTTL, scopeJournal)
	if err != nil {
		return err
	}
	if token == "" || token == attempt.PodToken {
		return fmt.Errorf("agentic merge journal token must be distinct from privileged pod token")
	}
	attempt.PlaneTokens.Journal = token
	return nil
}

func agenticMergeJournalEnv(cfg Config, attempt Attempt) []corev1.EnvVar {
	if !agenticMergeAuthority(attempt) {
		return nil
	}
	return []corev1.EnvVar{
		{Name: JournalEndpointEnv, Value: literalPodEnv(cfg.WriteAPIBase)},
		{Name: JournalTokenEnv, Value: literalPodEnv(attempt.PlaneTokens.Journal)},
	}
}
