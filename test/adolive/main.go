// Command adolive provisions the Azure DevOps scratch project that the live
// write leg (ADO-N16, #5727, .github/workflows/ado-live-write.yml) and the ADO
// provider fixture-drift leg (#4602, provider-fixture-drift-ado.yml) run
// against.
//
//	ADO_PAT=... go run ./test/adolive provision \
//	  -organization-url https://dev.azure.com/example-org \
//	  -project example-project -repository example-scratch [-apply]
//
// It is a dry run by default: it reads the scratch repository, its branch
// policies and the fixture work item, and prints what it would create. With
// -apply it creates only what is missing, so running it again changes nothing.
// It never updates or deletes anything, and it never creates a policy scoped by
// prefix: a blocking policy covering the leg's goobers-live/ branches would
// refuse every push the leg makes (design §8.2, F8). The token is read from
// ADO_PAT, never from a flag, and never printed.
//
// Finally it prints the repository variables to set. Only a repository admin
// can set them; this tool does not.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	tokenEnvironment = "ADO_PAT"
	apiVersion       = "7.1"

	// liveBranchProbe is a ref inside the live leg's namespace; any blocking
	// policy covering it would refuse the leg's pushes.
	liveBranchProbe = "refs/heads/goobers-live/probe"
	// Status policy identity; providers/ado_live_write_test.go publishes it.
	liveStatusGenre = "goobers-live"
	liveStatusName  = "live-write"

	fixtureTitle = "goobers provider fixture (do not close)"
	fixtureTag   = "goobers-fixture"
	fixtureBody  = "Stable seeded work item read by the ADO provider fixture-drift workflow (#4602). Do not edit or close it."
)

var errBlockingPrefix = errors.New("a blocking branch policy covers the live leg's goobers-live/ branches")

type config struct {
	organizationURL string
	project         string
	repository      string
	base            string
	fixtureType     string
	apply           bool
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, &http.Client{Timeout: 30 * time.Second})
	cancel()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, httpClient *http.Client) int {
	if len(args) == 0 || args[0] != "provision" {
		_, _ = fmt.Fprintln(stderr, "usage: adolive provision -organization-url URL -project NAME -repository NAME [-base main] [-fixture-type Issue] [-apply]")
		return 2
	}
	cfg, err := parseFlags(args[1:], stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	token := strings.TrimSpace(getenv(tokenEnvironment))
	if token == "" {
		_, _ = fmt.Fprintf(stderr, "%s is required; the token is never accepted as a flag\n", tokenEnvironment)
		return 2
	}
	c := &client{http: httpClient, organizationURL: strings.TrimRight(cfg.organizationURL, "/"), project: cfg.project, auth: basicAuth(token)}
	if err := provision(ctx, c, cfg, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "adolive:", err)
		return 1
	}
	return 0
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	flags := flag.NewFlagSet("provision", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var cfg config
	flags.StringVar(&cfg.organizationURL, "organization-url", "", "Azure DevOps organization URL")
	flags.StringVar(&cfg.project, "project", "", "Azure DevOps project")
	flags.StringVar(&cfg.repository, "repository", "", "dedicated scratch repository (never the testbed repository)")
	flags.StringVar(&cfg.base, "base", "main", "base branch the policies are scoped to")
	flags.StringVar(&cfg.fixtureType, "fixture-type", "Issue", "work item type for the #4602 fixture item")
	flags.BoolVar(&cfg.apply, "apply", false, "create what is missing (default: dry run)")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	for _, required := range []struct{ name, value string }{
		{"-organization-url", cfg.organizationURL},
		{"-project", cfg.project},
		{"-repository", cfg.repository},
		{"-base", cfg.base},
		{"-fixture-type", cfg.fixtureType},
	} {
		if strings.TrimSpace(required.value) == "" {
			return config{}, fmt.Errorf("%s is required", required.name)
		}
	}
	if u, err := url.Parse(cfg.organizationURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return config{}, fmt.Errorf("-organization-url must be an https URL")
	}
	return cfg, nil
}

func basicAuth(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+token))
}

// provision reads the project, creates what is missing when cfg.apply is set,
// and prints the repository variables to set.
func provision(ctx context.Context, c *client, cfg config, out io.Writer) error {
	repo, err := c.repository(ctx, cfg.repository)
	if err != nil {
		return err
	}
	printf(out, "Scratch repository %s (id %s)\n", repo.Name, repo.ID)
	ref := "refs/heads/" + strings.TrimPrefix(cfg.base, "refs/heads/")
	policyErr := provisionPolicies(ctx, c, cfg, repo, ref, out)
	fixtureID, err := provisionFixture(ctx, c, cfg, out)
	if err != nil {
		return err
	}
	printf(out, "\nRepository variables to set (an admin sets these; this tool does not):\n")
	printf(out, "  ADO_WRITE_REPOSITORY=%s\n", repo.Name)
	if fixtureID == 0 {
		printf(out, "  ADO_PROVIDER_FIXTURE_WORK_ITEM=<id printed by a run with -apply>\n")
	} else {
		printf(out, "  ADO_PROVIDER_FIXTURE_WORK_ITEM=%d\n", fixtureID)
	}
	if !cfg.apply {
		printf(out, "\nDry run: nothing was created. Re-run with -apply to create what is missing.\n")
	}
	return policyErr
}

func printf(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// desiredPolicy is one branch policy the scratch repository must carry.
type desiredPolicy struct {
	typeName string
	label    string
	settings map[string]any
	// identity lists settings that tell this policy apart from another of the
	// same type on the same ref.
	identity map[string]string
}

func desiredPolicies(repoID, ref string) []desiredPolicy {
	scope := []map[string]string{{"repositoryId": repoID, "refName": ref, "matchKind": "Exact"}}
	return []desiredPolicy{
		{
			typeName: "Minimum number of reviewers",
			label:    "minimum reviewers",
			settings: map[string]any{
				"minimumApproverCount": 1,
				"creatorVoteCounts":    false,
				"allowDownvotes":       false,
				"resetOnSourcePush":    false,
				"blockLastPusherVote":  true,
				"scope":                scope,
			},
		},
		{
			typeName: "Status",
			label:    "status " + liveStatusGenre + "/" + liveStatusName,
			settings: map[string]any{
				"statusGenre":              liveStatusGenre,
				"statusName":               liveStatusName,
				"invalidateOnSourceUpdate": true,
				"scope":                    scope,
			},
			identity: map[string]string{"statusGenre": liveStatusGenre, "statusName": liveStatusName},
		},
	}
}

func provisionPolicies(ctx context.Context, c *client, cfg config, repo repository, ref string, out io.Writer) error {
	types, err := c.policyTypes(ctx)
	if err != nil {
		return err
	}
	configs, err := c.policyConfigurations(ctx)
	if err != nil {
		return err
	}
	for _, want := range desiredPolicies(repo.ID, ref) {
		typeID, ok := types[strings.ToLower(want.typeName)]
		if !ok {
			return fmt.Errorf("policy type %q is not available in project %s", want.typeName, cfg.project)
		}
		if existing, found := findPolicy(configs, typeID, repo.ID, ref, want.identity); found {
			printf(out, "  policy %s on %s: present (id %d)\n", want.label, ref, existing.ID)
			continue
		}
		if !cfg.apply {
			printf(out, "  policy %s on %s: would create\n", want.label, ref)
			continue
		}
		created, err := c.createPolicy(ctx, typeID, want.settings)
		if err != nil {
			return fmt.Errorf("create policy %s: %w", want.label, err)
		}
		printf(out, "  policy %s on %s: created (id %d)\n", want.label, ref, created.ID)
	}
	if blocking := blockingPoliciesCovering(configs, repo.ID, liveBranchProbe); len(blocking) > 0 {
		for _, policy := range blocking {
			printf(out, "  push check: blocking policy %d (%s) covers refs/heads/goobers-live/; remove or re-scope it by hand\n", policy.ID, policy.Type.DisplayName)
		}
		return errBlockingPrefix
	}
	printf(out, "  push check: no blocking policy covers refs/heads/goobers-live/\n")
	return nil
}

// findPolicy reports an enabled, undeleted policy of typeID scoped exactly to
// repoID and ref whose identity settings match.
func findPolicy(configs []policyConfiguration, typeID, repoID, ref string, identity map[string]string) (policyConfiguration, bool) {
	for _, config := range configs {
		if config.IsDeleted || !config.IsEnabled || !strings.EqualFold(config.Type.ID, typeID) {
			continue
		}
		if !config.hasExactScope(repoID, ref) || !config.settingsMatch(identity) {
			continue
		}
		return config, true
	}
	return policyConfiguration{}, false
}

// blockingPoliciesCovering lists enabled, blocking policies whose scope
// includes ref in repoID — by exact name, by prefix, or with no ref at all.
func blockingPoliciesCovering(configs []policyConfiguration, repoID, ref string) []policyConfiguration {
	var covering []policyConfiguration
	for _, config := range configs {
		if config.IsDeleted || !config.IsEnabled || !config.IsBlocking {
			continue
		}
		for _, scope := range config.scopes() {
			if scope.covers(repoID, ref) {
				covering = append(covering, config)
				break
			}
		}
	}
	return covering
}

func provisionFixture(ctx context.Context, c *client, cfg config, out io.Writer) (int, error) {
	id, found, err := c.findFixture(ctx)
	if err != nil {
		return 0, err
	}
	switch {
	case found:
		printf(out, "Fixture work item (#4602): present #%d\n", id)
		return id, nil
	case !cfg.apply:
		printf(out, "Fixture work item (#4602): would create a %s titled %q\n", cfg.fixtureType, fixtureTitle)
		return 0, nil
	}
	id, err = c.createFixture(ctx, cfg.fixtureType)
	if err != nil {
		return 0, fmt.Errorf("create fixture work item: %w", err)
	}
	printf(out, "Fixture work item (#4602): created #%d\n", id)
	return id, nil
}

type repository struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type policyScope struct {
	RepositoryID string `json:"repositoryId"`
	RefName      string `json:"refName"`
	MatchKind    string `json:"matchKind"`
}

func (s policyScope) covers(repoID, ref string) bool {
	if s.RepositoryID != "" && !strings.EqualFold(s.RepositoryID, repoID) {
		return false
	}
	switch {
	case s.RefName == "":
		return true
	case strings.EqualFold(s.MatchKind, "Prefix"):
		return strings.HasPrefix(ref, s.RefName)
	default:
		return s.RefName == ref
	}
}

type policyConfiguration struct {
	ID         int  `json:"id"`
	IsEnabled  bool `json:"isEnabled"`
	IsBlocking bool `json:"isBlocking"`
	IsDeleted  bool `json:"isDeleted"`
	Type       struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	} `json:"type"`
	Settings json.RawMessage `json:"settings"`
}

func (p policyConfiguration) scopes() []policyScope {
	var settings struct {
		Scope []policyScope `json:"scope"`
	}
	if err := json.Unmarshal(p.Settings, &settings); err != nil {
		return nil
	}
	return settings.Scope
}

func (p policyConfiguration) hasExactScope(repoID, ref string) bool {
	for _, scope := range p.scopes() {
		if strings.EqualFold(scope.RepositoryID, repoID) && scope.RefName == ref && !strings.EqualFold(scope.MatchKind, "Prefix") {
			return true
		}
	}
	return false
}

func (p policyConfiguration) settingsMatch(identity map[string]string) bool {
	if len(identity) == 0 {
		return true
	}
	var settings map[string]any
	if err := json.Unmarshal(p.Settings, &settings); err != nil {
		return false
	}
	for key, want := range identity {
		if got, _ := settings[key].(string); !strings.EqualFold(got, want) {
			return false
		}
	}
	return true
}

// client is a minimal Azure DevOps REST client bound to one organization URL
// and project, both supplied by the caller.
type client struct {
	http            *http.Client
	organizationURL string
	project         string
	auth            string
}

func (c *client) endpoint(query url.Values, elems ...string) (string, error) {
	segments := make([]string, 0, len(elems)+2)
	for _, elem := range append([]string{c.project, "_apis"}, elems...) {
		segments = append(segments, url.PathEscape(elem))
	}
	joined, err := url.JoinPath(c.organizationURL, segments...)
	if err != nil {
		return "", err
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("api-version", apiVersion)
	return joined + "?" + query.Encode(), nil
}

// do sends one request and decodes a 2xx JSON response into out. It returns
// the response headers for continuation tokens.
func (c *client) do(ctx context.Context, method, endpoint, contentType string, body, out any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, redactQuery(endpoint), err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: status %d: %s", method, redactQuery(endpoint), resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("%s %s: decode: %w", method, redactQuery(endpoint), err)
		}
	}
	return resp.Header, nil
}

func redactQuery(endpoint string) string {
	before, _, _ := strings.Cut(endpoint, "?")
	return before
}

func (c *client) repository(ctx context.Context, name string) (repository, error) {
	endpoint, err := c.endpoint(nil, "git", "repositories", name)
	if err != nil {
		return repository{}, err
	}
	var repo repository
	if _, err := c.do(ctx, http.MethodGet, endpoint, "", nil, &repo); err != nil {
		return repository{}, fmt.Errorf("read scratch repository %q (create it first; this tool never creates or deletes repositories): %w", name, err)
	}
	if repo.ID == "" {
		return repository{}, fmt.Errorf("scratch repository %q has no id", name)
	}
	return repo, nil
}

// policyTypes maps each lowercased policy type display name to its id.
func (c *client) policyTypes(ctx context.Context) (map[string]string, error) {
	endpoint, err := c.endpoint(nil, "policy", "types")
	if err != nil {
		return nil, err
	}
	var out struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if _, err := c.do(ctx, http.MethodGet, endpoint, "", nil, &out); err != nil {
		return nil, err
	}
	types := make(map[string]string, len(out.Value))
	for _, t := range out.Value {
		types[strings.ToLower(t.DisplayName)] = t.ID
	}
	return types, nil
}

func (c *client) policyConfigurations(ctx context.Context) ([]policyConfiguration, error) {
	var all []policyConfiguration
	token := ""
	for range 100 {
		query := url.Values{}
		if token != "" {
			query.Set("continuationToken", token)
		}
		endpoint, err := c.endpoint(query, "policy", "configurations")
		if err != nil {
			return nil, err
		}
		var page struct {
			Value []policyConfiguration `json:"value"`
		}
		header, err := c.do(ctx, http.MethodGet, endpoint, "", nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
		if token = header.Get("X-Ms-Continuationtoken"); token == "" {
			return all, nil
		}
	}
	return nil, errors.New("policy configuration listing did not terminate")
}

func (c *client) createPolicy(ctx context.Context, typeID string, settings map[string]any) (policyConfiguration, error) {
	endpoint, err := c.endpoint(nil, "policy", "configurations")
	if err != nil {
		return policyConfiguration{}, err
	}
	body := map[string]any{
		"isEnabled":  true,
		"isBlocking": true,
		"type":       map[string]string{"id": typeID},
		"settings":   settings,
	}
	var created policyConfiguration
	_, err = c.do(ctx, http.MethodPost, endpoint, "application/json", body, &created)
	return created, err
}

// findFixture returns the lowest-numbered work item carrying the fixture title
// and tag.
func (c *client) findFixture(ctx context.Context) (int, bool, error) {
	endpoint, err := c.endpoint(url.Values{"$top": []string{"1"}}, "wit", "wiql")
	if err != nil {
		return 0, false, err
	}
	query := fmt.Sprintf(
		"SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project AND [System.Title] = '%s' AND [System.Tags] CONTAINS '%s' ORDER BY [System.Id] ASC",
		fixtureTitle, fixtureTag)
	var out struct {
		WorkItems []struct {
			ID int `json:"id"`
		} `json:"workItems"`
	}
	if _, err := c.do(ctx, http.MethodPost, endpoint, "application/json", map[string]string{"query": query}, &out); err != nil {
		return 0, false, err
	}
	if len(out.WorkItems) == 0 {
		return 0, false, nil
	}
	return out.WorkItems[0].ID, true, nil
}

func (c *client) createFixture(ctx context.Context, itemType string) (int, error) {
	endpoint, err := c.endpoint(nil, "wit", "workitems", "$"+itemType)
	if err != nil {
		return 0, err
	}
	patch := []map[string]string{
		{"op": "add", "path": "/fields/System.Title", "value": fixtureTitle},
		{"op": "add", "path": "/fields/System.Description", "value": fixtureBody},
		{"op": "add", "path": "/fields/System.Tags", "value": fixtureTag},
	}
	var created struct {
		ID int `json:"id"`
	}
	if _, err := c.do(ctx, http.MethodPost, endpoint, "application/json-patch+json", patch, &created); err != nil {
		return 0, err
	}
	if created.ID <= 0 {
		return 0, errors.New("created work item has no id: " + strconv.Itoa(created.ID))
	}
	return created.ID, nil
}
