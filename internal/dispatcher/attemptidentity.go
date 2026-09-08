package dispatcher

import "strconv"

// IdentityAttempt is the physical pod/surrender key. Number remains the
// invocation and journal ordinal, which can repeat on a graph repass.
func (a Attempt) IdentityAttempt() int {
	if a.PodAttempt > 0 {
		return a.PodAttempt
	}
	return a.Number
}

func (a Attempt) podAttemptEnv() string {
	if a.PodAttempt > 0 {
		return strconv.Itoa(a.PodAttempt)
	}
	return ""
}
