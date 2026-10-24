package configmirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/platform/durability"
)

// Client fetches sanitized worker configuration snapshots by digest.
type Client struct {
	BaseURL     string
	HTTP        *http.Client
	TokenSource func() (string, error)
}

// ResponseError reports a non-success config-plane response.
type ResponseError struct {
	StatusCode int
	Status     string
}

func (e *ResponseError) Error() string { return "config plane endpoint answered " + e.Status }

// Retryable reports a restart/failover response or transport failure.
func Retryable(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var response *ResponseError
	if errors.As(err, &response) {
		return response.StatusCode == http.StatusBadGateway ||
			response.StatusCode == http.StatusServiceUnavailable ||
			response.StatusCode == http.StatusGatewayTimeout
	}
	var network net.Error
	return errors.As(err, &network)
}

// Current reads the content digest the daemon currently publishes.
func (c Client) Current(ctx context.Context) (apicontract.ConfigSnapshotCurrent, error) {
	response, err := c.do(ctx, "")
	if err != nil {
		return apicontract.ConfigSnapshotCurrent{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return apicontract.ConfigSnapshotCurrent{}, &ResponseError{StatusCode: response.StatusCode, Status: response.Status}
	}
	var result apicontract.ConfigSnapshotCurrent
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	if err := decoder.Decode(&result); err != nil {
		return apicontract.ConfigSnapshotCurrent{}, err
	}
	if _, err := planeDigestName(result.Digest); err != nil || len(result.InstanceID) != 32 {
		return apicontract.ConfigSnapshotCurrent{}, errors.New("config plane returned invalid current metadata")
	}
	return result, nil
}

// Materialize fetches and validates digest into a private immutable cache.
func (c Client) Materialize(ctx context.Context, cacheRoot, digest string, validate func(string) error) (string, error) {
	name, err := planeDigestName(digest)
	if err != nil {
		return "", err
	}
	if validate == nil {
		return "", errors.New("config plane materialization requires validation")
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return "", err
	}
	destination := filepath.Join(cacheRoot, name)
	if info, err := os.Lstat(destination); err == nil {
		if !info.IsDir() {
			return "", errors.New("config snapshot cache entry is not a directory")
		}
		return destination, validate(destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	staging, err := os.MkdirTemp(cacheRoot, ".pending-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	archivePath := filepath.Join(staging, SnapshotName)
	if err := c.fetch(ctx, digest, archivePath); err != nil {
		return "", err
	}
	instanceRoot := filepath.Join(staging, "instance")
	if err := os.Mkdir(instanceRoot, 0o700); err != nil {
		return "", err
	}
	if err := ExtractFile(ctx, archivePath, instanceRoot); err != nil {
		return "", err
	}
	if err := validate(instanceRoot); err != nil {
		return "", err
	}
	if err := os.Remove(archivePath); err != nil {
		return "", err
	}
	if err := durability.Move(instanceRoot, destination); err != nil {
		return "", err
	}
	if err := durability.SyncDir(cacheRoot); err != nil {
		return "", err
	}
	return destination, nil
}

// GetBounded fetches one exact content-addressed config object into memory.
func (c Client) GetBounded(ctx context.Context, digest string, maximum int64) ([]byte, error) {
	if _, err := planeDigestName(digest); err != nil {
		return nil, err
	}
	if maximum <= 0 {
		return nil, errors.New("config plane read requires a positive bound")
	}
	response, err := c.do(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, &ResponseError{StatusCode: response.StatusCode, Status: response.Status}
	}
	if response.Header.Get(apicontract.DigestHeader) != digest {
		return nil, errors.New("config plane returned a different digest")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("config plane object exceeds size limit")
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("config plane object digest mismatch")
	}
	return data, nil
}

func (c Client) fetch(ctx context.Context, digest, destination string) error {
	response, err := c.do(ctx, digest)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return &ResponseError{StatusCode: response.StatusCode, Status: response.Status}
	}
	if response.Header.Get(apicontract.DigestHeader) != digest {
		return errors.New("config plane returned a different digest")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, MaxSnapshotBytes+(64<<20)+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > MaxSnapshotBytes+(64<<20) {
		return errors.New("config plane snapshot exceeds size limit")
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != digest {
		return errors.New("config plane snapshot digest mismatch")
	}
	return nil
}

func (c Client) do(ctx context.Context, digest string) (*http.Response, error) {
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + apicontract.ConfigSnapshotPath)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("config plane requires an absolute endpoint")
	}
	if digest != "" {
		query := base.Query()
		query.Set("digest", digest)
		base.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, err
	}
	if c.TokenSource != nil {
		token, err := c.TokenSource()
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{}
	}
	bounded := *client
	if bounded.Timeout <= 0 {
		bounded.Timeout = 2 * time.Minute
	}
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("config plane redirects are refused")
	}
	return bounded.Do(request)
}
