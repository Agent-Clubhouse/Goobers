package presentation

import "strconv"

const nanoAIUPerAIC = int64(1_000_000_000)

// FormatAIC renders a nano-AIU amount as nearest-whole AI credits.
func FormatAIC(nanoAIU int64) string {
	whole := nanoAIU / nanoAIUPerAIC
	remainder := nanoAIU % nanoAIUPerAIC
	if remainder >= nanoAIUPerAIC/2 {
		whole++
	} else if remainder <= -nanoAIUPerAIC/2 {
		whole--
	}
	return strconv.FormatInt(whole, 10) + " AIC"
}
