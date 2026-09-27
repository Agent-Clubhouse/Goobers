package rollup

import "testing"

// TestWorkItemRepositoryResolvesADOIdentity is #5266's identity half:
// workItemRepository understood only GitHub URLs, so every ADO row reported an
// unknown repository even when its receipt carried a URL — which is also why
// equal numeric ids in different ADO projects could not be told apart.
//
// The negative cases are the load-bearing ones. #5266 requires that a
// historical unknown identity stay explicitly unknown, so anything whose shape
// does not actually name the entity must still return "" rather than a partial
// guess that would render as a resolved repository.
func TestWorkItemRepositoryResolvesADOIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		url      string
		want     string
	}{
		{
			name: "ado work item is project scoped", provider: "ado",
			url:  "https://dev.azure.com/org/proj/_workitems/edit/7",
			want: "org/proj",
		},
		{
			name: "ado pull request is repository scoped", provider: "ado",
			url:  "https://dev.azure.com/org/proj/_git/repo/pullrequest/42",
			want: "org/proj/repo",
		},
		{
			// ADO Server is self-hosted on an arbitrary host. Constraining the
			// host to dev.azure.com would reintroduce the unknown-identity bug
			// for exactly the deployments that cannot use the cloud hostname,
			// so the PATH shape is what identifies the entity.
			name: "self-hosted ado server host still resolves", provider: "ado",
			url:  "https://tfs.corp.example:8080/org/proj/_workitems/edit/7",
			want: "org/proj",
		},
		{
			name: "ado provider with a github url stays unknown", provider: "ado",
			url: "https://github.com/acme/app/issues/7", want: "",
		},
		{
			name: "ado url missing the entity segment stays unknown", provider: "ado",
			url: "https://dev.azure.com/org/proj", want: "",
		},
		{
			name: "ado git url that is not a pull request stays unknown", provider: "ado",
			url: "https://dev.azure.com/org/proj/_git/repo/commit/abc123", want: "",
		},
		{
			name: "ado empty url stays unknown", provider: "ado", url: "", want: "",
		},
		{
			// ADO-N35: a legacy *.visualstudio.com URL carries the
			// organization in the host rather than as the path's leading
			// segment.
			name: "ado visualstudio.com work item resolves via host organization", provider: "ado",
			url:  "https://org.visualstudio.com/proj/_workitems/edit/7",
			want: "org/proj",
		},
		{
			name: "ado visualstudio.com pull request resolves via host organization", provider: "ado",
			url:  "https://org.visualstudio.com/proj/_git/repo/pullrequest/42",
			want: "org/proj/repo",
		},
		{
			name: "ado visualstudio.com host suffix is case-insensitive", provider: "ado",
			url:  "https://org.VISUALSTUDIO.COM/proj/_workitems/edit/7",
			want: "org/proj",
		},
		{
			// The organization keeps its case, as a dev.azure.com path
			// segment does, so neither host normalises it differently.
			name: "ado visualstudio.com organization keeps its case", provider: "ado",
			url:  "https://Org.visualstudio.com/proj/_workitems/edit/7",
			want: "Org/proj",
		},
		{
			name: "ado visualstudio.com DefaultCollection work item", provider: "ado",
			url:  "https://org.visualstudio.com/DefaultCollection/proj/_workitems/edit/7",
			want: "org/proj",
		},
		{
			name: "ado visualstudio.com DefaultCollection pull request", provider: "ado",
			url:  "https://org.visualstudio.com/DefaultCollection/proj/_git/repo/pullrequest/42",
			want: "org/proj/repo",
		},
		// GitHub behavior is re-pinned so widening the function cannot regress it.
		{
			name: "github issue", provider: "github",
			url: "https://github.com/acme/app/issues/7", want: "acme/app",
		},
		{
			name: "github pull request", provider: "github",
			url: "https://github.com/acme/app/pull/42", want: "acme/app",
		},
		{
			name: "github provider with a non-github host stays unknown", provider: "github",
			url: "https://ghe.corp.example/acme/app/issues/7", want: "",
		},
		{
			name: "unrecognized provider stays unknown", provider: "gitea",
			url: "https://gitea.example/acme/app/issues/7", want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workItemRepository(tc.provider, tc.url); got != tc.want {
				t.Errorf("workItemRepository(%q, %q) = %q, want %q", tc.provider, tc.url, got, tc.want)
			}
		})
	}
}

// TestWorkItemURLRoundTripsADOIdentity pins that the URL builder and the
// identity reader agree. They are separate functions on opposite sides of
// storage — one writes a fallback URL for a row whose receipt had none, the
// other reads identity back off a stored URL — so a disagreement between them
// would surface as an item whose repository and link describe different
// entities.
func TestWorkItemURLRoundTripsADOIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provider   string
		repository string
		kind       string
		externalID string
		want       string
	}{
		{
			name: "ado issue", provider: "ado", repository: "org/proj", kind: "issue", externalID: "7",
			want: "https://dev.azure.com/org/proj/_workitems/edit/7",
		},
		{
			name: "ado pr", provider: "ado", repository: "org/proj/repo", kind: "pr", externalID: "42",
			want: "https://dev.azure.com/org/proj/_git/repo/pullrequest/42",
		},
		{
			// A PR identity lacking its repository segment cannot name the
			// entity, so no URL is invented for it.
			name: "ado pr with only project scope stays unknown", provider: "ado",
			repository: "org/proj", kind: "pr", externalID: "42", want: "",
		},
		{
			name: "ado issue with a repository-scoped identity stays unknown", provider: "ado",
			repository: "org/proj/repo", kind: "issue", externalID: "7", want: "",
		},
		{
			name: "github issue", provider: "github", repository: "acme/app", kind: "issue", externalID: "7",
			want: "https://github.com/acme/app/issues/7",
		},
		{
			name: "github pr", provider: "github", repository: "acme/app", kind: "pr", externalID: "42",
			want: "https://github.com/acme/app/pull/42",
		},
		{
			name: "empty repository stays unknown", provider: "github", repository: "", kind: "issue",
			externalID: "7", want: "",
		},
		{
			// An empty id yields the shared URL PREFIX, not "". RelatedPullRequests
			// depends on that form for its SQL LIKE, so guarding it away would
			// silently return no related pull requests.
			name: "ado empty id yields the kind prefix", provider: "ado", repository: "org/proj",
			kind: "issue", externalID: "", want: "https://dev.azure.com/org/proj/_workitems/edit/",
		},
		{
			name: "github empty id yields the kind prefix", provider: "github", repository: "acme/app",
			kind: "pr", externalID: "", want: "https://github.com/acme/app/pull/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workItemURL(tc.provider, tc.repository, tc.kind, tc.externalID)
			if got != tc.want {
				t.Fatalf("workItemURL(%q, %q, %q, %q) = %q, want %q",
					tc.provider, tc.repository, tc.kind, tc.externalID, got, tc.want)
			}
			// Where a full entity URL was produced, reading identity back off it
			// must return the identity it was built from. A bare prefix (empty
			// id) names no entity, so it is not expected to round-trip.
			if got == "" || tc.externalID == "" {
				return
			}
			if back := workItemRepository(tc.provider, got); back != tc.repository {
				t.Errorf("workItemRepository(%q, %q) = %q, want the original %q",
					tc.provider, got, back, tc.repository)
			}
		})
	}
}
