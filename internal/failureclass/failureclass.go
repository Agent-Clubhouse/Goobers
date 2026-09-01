// Package failureclass recognizes the shape of a command failure that a
// dependency fetch caused rather than the work under test.
//
// It is its own package because two layers need the same vocabulary and
// cannot share it any other way: internal/gate classifies a finished stage
// result so an infrastructure fault is not charged to the item (#3373), and
// internal/executor decides which line of a failing command's output becomes
// that result's recorded diagnostic in the first place. internal/gate already
// imports internal/executor, so the vocabulary cannot live in either of them.
package failureclass

import "strings"

// dependencyFetchMarkers say the failing command was a dependency or
// artifact fetch: a module-proxy/package-registry host, or a toolchain's own
// download diagnostic.
//
// The npm entries are the tool's own error prefix rather than a host, because
// a lockfile can name any mirror — the private feed in #4141 was not, and
// could not be, on a host list.
var dependencyFetchMarkers = []string{
	"go: downloading",
	"go: module ",
	"go mod download",
	"verifying module",
	"reading https://",
	"goproxy",
	"proxy.golang.org",
	"sum.golang.org",
	"storage.googleapis.com",
	"registry.npmjs.org",
	"npm error",
	"npm err!",
	"files.pythonhosted.org",
	"pypi.org",
	"index.crates.io",
}

// transportDenialTokens say the network refused or could not reach that
// fetch — an egress policy denial or an unreachable proxy, neither of which
// any diff can fix.
var transportDenialTokens = []string{
	"403 forbidden",
	": forbidden",
	"connection refused",
	"econnrefused",
	"i/o timeout",
	"etimedout",
	"tls handshake timeout",
	"proxyconnect",
	"network is unreachable",
}

// IsDependencyTransportDenial reports whether message is a dependency or
// artifact fetch that the network refused (#3373: an egress proxy answering
// Forbidden to a module zip fetch classified as a code failure and cost six
// implement repasses). Both axes are required: a 403 or a refused connection
// on its own is ordinary application output, and a package host on its own is
// ordinary build chatter. Matching is case-insensitive.
func IsDependencyTransportDenial(message string) bool {
	message = strings.ToLower(message)
	return containsAny(message, dependencyFetchMarkers) &&
		containsAny(message, transportDenialTokens)
}

func containsAny(message string, tokens []string) bool {
	for _, token := range tokens {
		if strings.Contains(message, token) {
			return true
		}
	}
	return false
}
