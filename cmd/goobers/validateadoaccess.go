package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// Read-only Azure DevOps access checks run by `validate --check-repos` after
// every repository is reachable (docs/design/ado-parity-dsl-2-0.md §7.3,
// ADO-N34). They report the identity the configured credential authenticates
// as, fail on a missing permission Goobers needs, and warn on access a
// customer's branch policy rules forbid Goobers to hold. Nothing here writes.
const (
	adoAccessMissingPermissionCode = "ADOACCESS001"
	adoAccessForcePushCode         = "ADOACCESS002"
	adoAccessBypassCode            = "ADOACCESS003"
	adoAccessBlanketPolicyCode     = "ADOACCESS004"
	adoAccessUnknownCode           = "ADOACCESS005"
	adoBacklogStatesCode           = "BACKLOG002"
)

// adoRequiredGitPermissions are the Git repositories permissions without
// which Goobers cannot push a run branch or open a pull request.
var adoRequiredGitPermissions = []providers.ADOGitPermission{
	providers.ADOGitContribute,
	providers.ADOGitPullRequestContribute,
	providers.ADOGitCreateBranch,
}

// adoBypassGitPermissions let an identity land changes past branch policy.
// Goobers never bypasses policy, so holding either is a warning.
var adoBypassGitPermissions = []providers.ADOGitPermission{
	providers.ADOGitPullRequestPolicyOverride,
	providers.ADOGitPolicyExempt,
}

func adoEvaluatedGitPermissions() []providers.ADOGitPermission {
	permissions := append([]providers.ADOGitPermission{}, adoRequiredGitPermissions...)
	permissions = append(permissions, providers.ADOGitForcePush)
	return append(permissions, adoBypassGitPermissions...)
}

// adoRepositoryAccess is what the read-only access reads found. Each read
// keeps its own error so one refused read (for example a credential that
// cannot evaluate permissions) leaves the others reported.
type adoRepositoryAccess struct {
	identity       providers.ADOIdentity
	identityErr    error
	permissions    map[providers.ADOGitPermission]bool
	permissionsErr error
	policies       []providers.ADOBranchPolicy
	policiesErr    error
}

// adoErrScrubber returns a func that redacts secrets from an ADO error message.
func adoErrScrubber(registry *journal.RegistryScrubber) func(error) error {
	return func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%s", journal.Chain(registry, journal.NewPatternScrubber()).Scrub([]byte(err.Error())))
	}
}

// targetADORepositoryAccess performs the reads. A package var so tests stub
// the provider and never leave the process.
var targetADORepositoryAccess = readADORepositoryAccess

func readADORepositoryAccess(ctx context.Context, repo instance.RepoRef, stores credentials.StoreResolver) adoRepositoryAccess {
	registry := journal.NewRegistryScrubber()
	scrub := adoErrScrubber(registry)
	provider, err := adoauth.Provider(repo, nil, registry, nil, nil, stores)
	if err != nil {
		err = scrub(err)
		return adoRepositoryAccess{identityErr: err, permissionsErr: err, policiesErr: err}
	}
	ref := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
	var access adoRepositoryAccess
	access.identity, err = provider.AuthenticatedIdentity(ctx)
	access.identityErr = scrub(err)
	ids, err := provider.RepositoryIDs(ctx, ref)
	if err != nil {
		access.permissionsErr, access.policiesErr = scrub(err), scrub(err)
		return access
	}
	access.permissions, err = provider.EvaluateGitPermissions(ctx, ids, adoEvaluatedGitPermissions())
	access.permissionsErr = scrub(err)
	access.policies, err = provider.BlanketPrefixPolicies(ctx, ref, ids.RepositoryID)
	access.policiesErr = scrub(err)
	return access
}

// adoAccessReporter prints one repository's findings and records them as
// diagnostics against its instance.yaml entry.
type adoAccessReporter struct {
	label       string
	file        string
	path        string
	stdout      io.Writer
	diagnostics *diagnosticCollector
}

func (r adoAccessReporter) warn(code, message string) {
	pf(r.stdout, "REPOSITORY %s: WARNING %s: %s\n", r.label, code, message)
	r.diagnostics.add(r.file, r.path, code, string(validate.Warning), r.label+": "+message)
}

func (r adoAccessReporter) fail(code, message string) {
	pf(r.stdout, "ERROR %s %s: %s\n", code, r.label, message)
	r.diagnostics.add(r.file, r.path, code, string(validate.Error), r.label+": "+message)
}

// checkADORepositoryAccess runs the read-only access checks on every Azure
// DevOps repository. It returns false when a required permission is denied;
// every other finding is a warning.
func checkADORepositoryAccess(repos []instance.RepoRef, stores credentials.StoreResolver, stdout io.Writer, file string, diagnostics *diagnosticCollector) bool {
	ok := true
	for i, repo := range repos {
		if repo.Provider != string(providers.ProviderADO) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
		access := targetADORepositoryAccess(ctx, repo, stores)
		cancel()
		reporter := adoAccessReporter{
			label:       fmt.Sprintf("repos[%d] %s/%s", i, repo.Owner, repo.Name),
			file:        file,
			path:        fmt.Sprintf("/repos/%d", i),
			stdout:      stdout,
			diagnostics: diagnostics,
		}
		reportADOIdentity(reporter, access)
		if !reportADOPermissions(reporter, access) {
			ok = false
		}
		reportADOBlanketPolicies(reporter, access)
	}
	return ok
}

func reportADOIdentity(r adoAccessReporter, access adoRepositoryAccess) {
	if access.identityErr != nil {
		r.warn(adoAccessUnknownCode, fmt.Sprintf("could not read the authenticated identity: %v", access.identityErr))
		return
	}
	upn := access.identity.UniqueName
	if upn == "" {
		upn = "no UPN"
	}
	pf(r.stdout, "REPOSITORY %s: authenticated as ADO identity %s (%s)\n", r.label, access.identity.ID, upn)
}

// reportADOPermissions reports the permission evaluation. A permission the
// evaluation could not read is unknown, never missing.
func reportADOPermissions(r adoAccessReporter, access adoRepositoryAccess) bool {
	if access.permissionsErr != nil {
		r.warn(adoAccessUnknownCode, fmt.Sprintf("could not evaluate Git permissions: %v; permissions are unknown, not missing", access.permissionsErr))
		return true
	}
	missing := adoPermissionsWithValue(access.permissions, adoRequiredGitPermissions, false)
	ok := len(missing) == 0
	if !ok {
		r.fail(adoAccessMissingPermissionCode, fmt.Sprintf(
			"the identity lacks %s on this repository; Goobers needs Contribute, Contribute to pull requests and Create branch to create and push run branches and open pull requests",
			strings.Join(missing, ", ")))
	}
	if !access.permissions[providers.ADOGitForcePush] {
		r.warn(adoAccessForcePushCode, fmt.Sprintf("the identity lacks %q at repository level; Azure DevOps lets a branch's creator force-push its own branches, but rewriting or deleting branches it did not create (for example remediating a human-opened pull request) will fail",
			providers.ADOGitForcePush.String()))
	}
	if held := adoPermissionsWithValue(access.permissions, adoBypassGitPermissions, true); len(held) > 0 {
		r.warn(adoAccessBypassCode, fmt.Sprintf(
			"the identity holds %s; Goobers never bypasses branch policy, and its identity should not be able to (remove the allow)",
			strings.Join(held, ", ")))
	}
	if ok {
		pf(r.stdout, "REPOSITORY %s: required Git permissions held\n", r.label)
	}
	return ok
}

// adoPermissionsWithValue returns, quoted, the names of the permissions whose
// evaluated value is want.
func adoPermissionsWithValue(evaluated map[providers.ADOGitPermission]bool, permissions []providers.ADOGitPermission, want bool) []string {
	var names []string
	for _, permission := range permissions {
		if evaluated[permission] == want {
			names = append(names, strconv.Quote(permission.String()))
		}
	}
	return names
}

func reportADOBlanketPolicies(r adoAccessReporter, access adoRepositoryAccess) {
	if access.policiesErr != nil {
		r.warn(adoAccessUnknownCode, fmt.Sprintf("could not read branch policies: %v; blanket policies are unknown, not absent", access.policiesErr))
		return
	}
	for _, policy := range access.policies {
		r.warn(adoAccessBlanketPolicyCode, fmt.Sprintf(
			"blocking policy %d (%s) has a Prefix scope %q covering every branch; every push to refs/heads/ must go through a pull request, so Goobers cannot push run branches (scope the policy to the target branch instead)",
			policy.ID, policy.TypeName, policy.RefName))
	}
}

// adoBacklogStates is what the Boards state reads found for one gaggle: the
// resolved create type with its states, and the states of each work item type
// its backlog.doneStates.byType names.
type adoBacklogStates struct {
	createType   string
	createStates []providers.ADOWorkItemState
	createErr    error
	byType       map[string][]providers.ADOWorkItemState
	byTypeErrs   map[string]error
}

// targetADOBacklogStates performs the Boards state reads. A package var so
// tests stub the provider.
var targetADOBacklogStates = readADOBacklogStates

func readADOBacklogStates(ctx context.Context, repo instance.RepoRef, project string, types []string, stores credentials.StoreResolver) adoBacklogStates {
	registry := journal.NewRegistryScrubber()
	scrub := adoErrScrubber(registry)
	out := adoBacklogStates{byType: map[string][]providers.ADOWorkItemState{}, byTypeErrs: map[string]error{}}
	provider, err := adoauth.Provider(repo, nil, registry, nil, nil, stores)
	if err != nil {
		out.createErr = scrub(err)
		for _, itemType := range types {
			out.byTypeErrs[itemType] = out.createErr
		}
		return out
	}
	ref := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: repo.Owner, Project: project, Name: repo.Name}
	out.createType, err = provider.DefaultCreateType(ctx, ref)
	if err == nil {
		out.createStates, err = provider.WorkItemTypeStates(ctx, ref, out.createType)
	}
	out.createErr = scrub(err)
	for _, itemType := range types {
		states, err := provider.WorkItemTypeStates(ctx, ref, itemType)
		if err != nil {
			out.byTypeErrs[itemType] = scrub(err)
			continue
		}
		out.byType[itemType] = states
	}
	return out
}

// checkADOBacklogStates reads the Boards project's real per-type states and
// checks the gaggle's configuration against them. Advisory: every finding is
// a warning.
func checkADOBacklogStates(gaggleName string, byType map[string][]string, repo instance.RepoRef, project string, stores credentials.StoreResolver, stdout io.Writer, file string, diagnostics *diagnosticCollector) {
	types := make([]string, 0, len(byType))
	for itemType := range byType {
		types = append(types, itemType)
	}
	sort.Strings(types)
	ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
	states := targetADOBacklogStates(ctx, repo, project, types, stores)
	cancel()
	warn := func(path, message string) {
		pf(stdout, "BACKLOG Gaggle/%s: WARNING %s: %s\n", gaggleName, adoBacklogStatesCode, message)
		diagnostics.add(file, path, adoBacklogStatesCode, string(validate.Warning), fmt.Sprintf("Gaggle/%s: %s", gaggleName, message))
	}
	reportADOCreateTypeStates(gaggleName, states, stdout, warn)
	warnCaseDuplicateADOTypes(types, warn)
	for _, itemType := range types {
		path := "/spec/backlog/doneStates/byType/" + itemType
		if err := states.byTypeErrs[itemType]; err != nil {
			warn(path, fmt.Sprintf("could not read the states of work item type %q in project %q: %v; check the type name", itemType, project, err))
			continue
		}
		for _, name := range unknownADOStateNames(byType[itemType], states.byType[itemType]) {
			warn(path, fmt.Sprintf("backlog.doneStates.byType names state %q, which work item type %q does not have (its states: %s)",
				name, itemType, adoStateNames(states.byType[itemType])))
		}
	}
}

// warnCaseDuplicateADOTypes warns about backlog.doneStates.byType keys that
// name the same work item type in different case (ADO type names are
// case-insensitive). The provider merges them, but two keys usually mean a
// typo. types is sorted.
func warnCaseDuplicateADOTypes(types []string, warn func(path, message string)) {
	first := make(map[string]string, len(types))
	for _, itemType := range types {
		folded := strings.ToLower(strings.TrimSpace(itemType))
		if earlier, ok := first[folded]; ok {
			warn("/spec/backlog/doneStates/byType/"+itemType, fmt.Sprintf(
				"backlog.doneStates.byType keys %q and %q name the same work item type; their states are merged", earlier, itemType))
			continue
		}
		first[folded] = itemType
	}
}

func reportADOCreateTypeStates(gaggleName string, states adoBacklogStates, stdout io.Writer, warn func(path, message string)) {
	if states.createErr != nil {
		warn("/spec/backlog/project", fmt.Sprintf("could not read the create work item type's states: %v; state names are unchecked", states.createErr))
		return
	}
	for _, state := range states.createStates {
		if strings.EqualFold(state.Category, "Completed") {
			pf(stdout, "BACKLOG Gaggle/%s: create type %q states: %s\n", gaggleName, states.createType, adoStateNames(states.createStates))
			return
		}
	}
	warn("/spec/backlog/project", fmt.Sprintf("create type %q has no state in the Completed category, so Goobers cannot close its items (its states: %s)",
		states.createType, adoStateNames(states.createStates)))
}

func unknownADOStateNames(names []string, states []providers.ADOWorkItemState) []string {
	var unknown []string
	for _, name := range names {
		found := false
		for _, state := range states {
			if strings.EqualFold(strings.TrimSpace(name), state.Name) {
				found = true
				break
			}
		}
		if !found {
			unknown = append(unknown, name)
		}
	}
	return unknown
}

func adoStateNames(states []providers.ADOWorkItemState) string {
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, state.Name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
