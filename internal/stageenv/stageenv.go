// Package stageenv is the environment a stage process reads its delivered
// credentials from (#6610): the GOOBERS_CRED_<capability> values and the
// scheme, expiry and refresh-grant variables the daemon sets beside them.
//
// Production reads the process environment. A test supplies a Lookup over
// its own variables instead of calling t.Setenv, which mutates process-wide
// state and so forbids t.Parallel.
package stageenv

import "os"

// Lookup reports an environment variable's value and whether it is set, with
// os.LookupEnv's contract. The nil Lookup is the process environment.
type Lookup func(key string) (string, bool)

// Get returns key's value, or "" when it is unset.
func (l Lookup) Get(key string) string {
	if l == nil {
		return os.Getenv(key)
	}
	value, _ := l(key)
	return value
}
