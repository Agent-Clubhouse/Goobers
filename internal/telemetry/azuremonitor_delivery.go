package telemetry

// Delivery evidence for one Azure Monitor replay stream (#5940): when the
// destination last acknowledged a batch, when delivery last failed, and a
// fixed failure class. Like the health channel that reports it, this carries
// no endpoint URL, response body, record content or raw error text.
import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// The delivery failure vocabulary. It is a stable query surface: add a class
// only with its KQL/docs, never derive one from error text.
const (
	azureDeliveryRejected  = "rejected"
	azureDeliveryTimeout   = "timeout"
	azureDeliveryDNS       = "dns"
	azureDeliveryTLS       = "tls"
	azureDeliveryNetwork   = "network"
	azureDeliverySpoolIO   = "spool_io"
	azureDeliveryMalformed = "malformed"
	azureDeliveryUnknown   = "unknown"
)

const (
	azureDeliveryStatusSchema = "goobers.dev/telemetry/azure-delivery-status/v1"
	azureDeliveryStatusLimit  = 4096
)

// azureMonitorRejectedError is an HTTP answer from the destination that was
// not a full acknowledgement. The status code is the only retained detail.
type azureMonitorRejectedError struct {
	status  int
	partial bool
}

func (e *azureMonitorRejectedError) Error() string {
	if e.partial {
		return fmt.Sprintf("the Azure Monitor destination partially rejected telemetry (HTTP %d)", e.status)
	}
	return fmt.Sprintf("the Azure Monitor destination rejected telemetry (HTTP %d)", e.status)
}

// azureMonitorMalformedError is a spooled payload that could not be projected
// for delivery. Retrying the same bytes cannot succeed.
type azureMonitorMalformedError struct{ cause error }

func (e *azureMonitorMalformedError) Error() string { return e.cause.Error() }
func (e *azureMonitorMalformedError) Unwrap() error { return e.cause }

// classifyAzureDelivery maps a send failure onto the fixed vocabulary. Order
// matters: a TLS or DNS failure surfaces wrapped in url.Error/net.OpError, and
// a DNS lookup timeout is reported as a timeout.
func classifyAzureDelivery(err error) string {
	if err == nil {
		return ""
	}
	var malformed *azureMonitorMalformedError
	var rejected *azureMonitorRejectedError
	var dns *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &malformed):
		return azureDeliveryMalformed
	case errors.As(err, &rejected):
		return azureDeliveryRejected
	case isAzureDeliveryTLSError(err):
		return azureDeliveryTLS
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.As(err, &netErr) && netErr.Timeout():
		return azureDeliveryTimeout
	case errors.As(err, &dns):
		return azureDeliveryDNS
	case isAzureDeliveryNetworkError(err):
		return azureDeliveryNetwork
	default:
		return azureDeliveryUnknown
	}
}

func isAzureDeliveryTLSError(err error) bool {
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &verification) || errors.As(err, &record) || errors.As(err, &alert) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

func isAzureDeliveryNetworkError(err error) bool {
	var op *net.OpError
	var transport *url.Error
	return errors.As(err, &op) || errors.As(err, &transport) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}

// azureDeliveryStatus is the part of delivery evidence that survives a
// restart. Whether a failure is still active does not: it is re-established
// by the next attempt, so an idle stream does not warn forever about a
// failure from a previous process.
type azureDeliveryStatus struct {
	Schema       string    `json:"schema,omitempty"`
	LastSuccess  time.Time `json:"lastSuccess,omitzero"`
	LastFailure  time.Time `json:"lastFailure,omitzero"`
	FailureClass string    `json:"failureClass,omitempty"`
}

// merge keeps the later success and the later failure with its class.
func (s azureDeliveryStatus) merge(other azureDeliveryStatus) azureDeliveryStatus {
	if other.LastSuccess.After(s.LastSuccess) {
		s.LastSuccess = other.LastSuccess
	}
	if other.LastFailure.After(s.LastFailure) {
		s.LastFailure, s.FailureClass = other.LastFailure, other.FailureClass
	}
	return s
}

type azureDeliveryState struct {
	mu        sync.Mutex
	status    azureDeliveryStatus
	active    bool
	persisted azureDeliveryStatus
}

func (d *azureDeliveryState) succeeded(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status.LastSuccess = now.UTC()
	d.active = false
}

// failed records a failure. active marks a delivery-path failure that holds
// the stream back until a later delivery succeeds; admission and malformed
// records are reported through their own loss counters instead.
// While a delivery failure is active, its class stays the reported one; a
// passing non-active failure is visible only through its loss counters.
func (d *azureDeliveryState) failed(now time.Time, class string, active bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !active && d.active {
		return
	}
	d.status.LastFailure, d.status.FailureClass = now.UTC(), class
	if active {
		d.active = true
	}
}

// spoolReadable clears an active spool_io failure once the manifest answers
// again, so an idle stream with nothing left to send does not keep warning.
func (d *azureDeliveryState) spoolReadable() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active && d.status.FailureClass == azureDeliverySpoolIO {
		d.active = false
	}
}

func (d *azureDeliveryState) snapshot() (azureDeliveryStatus, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status, d.active
}

func (d *azureDeliveryState) restore(status azureDeliveryStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status = d.status.merge(status)
	d.persisted = d.status
}

// persist writes the restart-surviving evidence when it changed. It runs from
// the health sampler, so a busy stream writes at most once per sample.
func (d *azureDeliveryState) persist(root, stream string) {
	d.mu.Lock()
	current := d.status
	changed := current != d.persisted
	d.mu.Unlock()
	if !changed {
		return
	}
	if err := writeAzureDeliveryStatus(root, stream, current); err != nil {
		return // Best effort: the next sample retries.
	}
	d.mu.Lock()
	d.persisted = current
	d.mu.Unlock()
}

func azureDeliveryStatusPath(root, stream string) (string, error) {
	if stream != "journal" && stream != "diagnostics" && stream != "traces" && stream != "export" {
		return "", fmt.Errorf("invalid delivery status stream")
	}
	return filepath.Join(root, "status-"+stream+".json"), nil
}

func readAzureDeliveryStatus(root, stream string) (azureDeliveryStatus, error) {
	path, err := azureDeliveryStatusPath(root, stream)
	if err != nil {
		return azureDeliveryStatus{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return azureDeliveryStatus{}, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, azureDeliveryStatusLimit+1))
	if err != nil {
		return azureDeliveryStatus{}, err
	}
	var status azureDeliveryStatus
	if len(data) > azureDeliveryStatusLimit || json.Unmarshal(data, &status) != nil || status.Schema != azureDeliveryStatusSchema {
		return azureDeliveryStatus{}, errors.New("invalid delivery status")
	}
	if !knownAzureDeliveryClass(status.FailureClass) {
		status.FailureClass = azureDeliveryUnknown
	}
	return status, nil
}

func knownAzureDeliveryClass(class string) bool {
	switch class {
	case "", azureDeliveryRejected, azureDeliveryTimeout, azureDeliveryDNS, azureDeliveryTLS,
		azureDeliveryNetwork, azureDeliverySpoolIO, azureDeliveryMalformed, azureDeliveryUnknown:
		return true
	}
	return false
}

// writeAzureDeliveryStatus merges with the file a sibling process sharing the
// spool root may have written, then replaces it atomically.
func writeAzureDeliveryStatus(root, stream string, status azureDeliveryStatus) error {
	path, err := azureDeliveryStatusPath(root, stream)
	if err != nil {
		return err
	}
	lock, err := platformlock.TryAcquire(path + ".lock")
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	if err = lock.File().Chmod(0o600); err != nil {
		return err
	}
	if prior, readErr := readAzureDeliveryStatus(root, stream); readErr == nil {
		status = status.merge(prior)
	}
	status.Schema = azureDeliveryStatusSchema
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".status-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
