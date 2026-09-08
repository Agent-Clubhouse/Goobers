package recovery

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// HTTPArchiveSource downloads recovery state using the receiving run's claims
// bearer, never a journal or surrender bearer. The server authorizes the issue
// claim and selects the source run; callers cannot request arbitrary source IDs.
type HTTPArchiveSource struct {
	BaseURL string
	Token   string
	RunID   string
	Client  *http.Client
}

// WithArchive verifies a bounded streamed envelope in private temporary storage
// and invokes consume only for an unexpired record in the requested repository.
// consume must import/restore the Git objects before returning: its archive path
// is removed on every exit. No unverified bytes are written into a worktree.
func (s HTTPArchiveSource) WithArchive(ctx context.Context, repositoryKey, issueID string, consume func(Record, string) error) error {
	endpoint, err := s.endpoint(repositoryKey, issueID)
	if err != nil {
		return err
	}
	if consume == nil {
		return fmt.Errorf("recovery download requires a consumer")
	}
	// Longer than the server's 60-second blob budget, including body streaming.
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("invalid recovery download request")
	}
	request.Header.Set("Authorization", "Bearer "+s.Token)
	request.Header.Set("Accept", "application/octet-stream")
	client := http.Client{}
	if s.Client != nil {
		client = *s.Client
	}
	// Even same-origin redirects can change the run/issue authorization target.
	// Do not forward a claims bearer to any redirected URL.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Transport errors may contain credential-bearing URLs; do not echo them.
		return fmt.Errorf("recovery download transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("recovery download refused (HTTP %d)", response.StatusCode)
	}
	directory, err := os.MkdirTemp("", "goobers-recovery-download-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	record, err := ReceiveArchiveEnvelope(ctx, response.Body, directory, 512<<20)
	if err != nil {
		return err
	}
	if record.RepositoryKey != repositoryKey || record.RunID == s.RunID || !time.Now().Before(record.RetainUntil) {
		return fmt.Errorf("recovery download identity or retention mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return consume(record, filepath.Join(directory, BundleFileName))
}

func (s HTTPArchiveSource) endpoint(repositoryKey, issueID string) (string, error) {
	base, err := url.Parse(s.BaseURL)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", fmt.Errorf("invalid recovery API endpoint")
	}
	if !runIdentity.MatchString(s.RunID) || strings.TrimSpace(s.Token) == "" || strings.ContainsAny(s.Token, "\r\n") || repositoryKey == "" || len(repositoryKey) > 4096 || issueID == "" || len(issueID) > 256 {
		return "", fmt.Errorf("recovery download requires a run, claims bearer, repository and issue")
	}
	base.Path = strings.TrimRight(base.Path, "/") + strings.ReplaceAll(apicontract.RunRecoveryPath, "{run}", s.RunID)
	base.RawPath = ""
	query := url.Values{"repositoryKey": {repositoryKey}, "issue": {issueID}}
	base.RawQuery = query.Encode()
	return base.String(), nil
}
