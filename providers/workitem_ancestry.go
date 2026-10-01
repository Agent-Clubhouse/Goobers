package providers

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// Work-item ancestry (#6125): a bounded, provider-neutral walk up the
// provider-native parent relation of the work items a pull request closes.
// Providers implement only WorkItemParentReader, the narrow relation read;
// TraverseWorkItemAncestry owns the bounds, cycle detection, type filter,
// cross-project policy and deterministic ordering, so every provider's
// ancestry obeys one contract.

// Ancestry omission reasons. Each names why a parent is absent from
// WorkItemAncestry.Items.
const (
	// AncestryOmitMaxDepth: the parent lies beyond the configured depth.
	AncestryOmitMaxDepth = "max-depth"
	// AncestryOmitMaxItems: reading the parent would exceed the item budget.
	AncestryOmitMaxItems = "max-items"
	// AncestryOmitCycle: the parent is already an ancestor of its own chain.
	AncestryOmitCycle = "cycle"
	// AncestryOmitCrossProject: the parent lives in another project or
	// repository and the policy denies crossing it.
	AncestryOmitCrossProject = "cross-project"
	// AncestryOmitExcludedType: the parent's type is not in the include
	// list. Traversal still continues through it.
	AncestryOmitExcludedType = "excluded-type"
	// AncestryOmitNotFound: the parent no longer exists, or the provider
	// omitted it without saying why.
	AncestryOmitNotFound = "not-found"
	// AncestryOmitAccessDenied: the credential cannot read the parent.
	AncestryOmitAccessDenied = "access-denied"
	// AncestryOmitReadFailed: the parent read failed for another reason.
	AncestryOmitReadFailed = "read-failed"
)

// Ancestry collection statuses.
const (
	// AncestryComplete: every parent within the walk was read and included
	// (type-filtered parents do not make a walk incomplete).
	AncestryComplete = "complete"
	// AncestryIncomplete: at least one parent was omitted by a bound, a
	// policy, a cycle or a failed read; Omissions says which and why.
	AncestryIncomplete = "incomplete"
	// AncestryUnsupported: the provider has no native parent relation reader.
	AncestryUnsupported = "unsupported"
)

// Ancestry cross-project policies.
const (
	AncestryCrossProjectDeny  = "deny"
	AncestryCrossProjectAllow = "allow"
)

// WorkItemField is one selected, provider-authored work-item field.
type WorkItemField struct {
	Name      string
	Value     string
	Truncated bool
}

// WorkItemNode is one work item in an ancestry walk: its qualified identity,
// the few fields that carry intent, and its immediate parent when the
// provider reports that inline. It never carries a raw provider payload.
type WorkItemNode struct {
	Provider ProviderKind
	// Project is the item's container: the Azure DevOps project, or the
	// GitHub "owner/name" repository.
	Project string
	ID      string
	Type    string
	Title   string
	State   string
	URL     string
	Fields  []WorkItemField
	// ParentID is the immediate parent's id when the provider reports the
	// parent relation inline with the item (Azure DevOps). ParentUnknown is
	// set instead when only a separate read can tell (GitHub).
	ParentID      string
	ParentUnknown bool
	Integrity     apiintegrity.Grade
}

// Key is the node's stable qualified identity: provider:project:id.
func (n WorkItemNode) Key() string {
	return string(n.Provider) + ":" + n.Project + ":" + n.ID
}

func (n WorkItemNode) mayHaveParent() bool {
	return n.ParentID != "" || n.ParentUnknown
}

// WorkItemParentRead is the outcome of reading one child's immediate parent.
// Parent is nil with an empty Omission when the child has no parent.
type WorkItemParentRead struct {
	Parent   *WorkItemNode
	ParentID string
	Omission string
	Detail   string
}

// WorkItemParentReader is the narrow relation reader bounded ancestry is
// built on. AncestryRoot turns a directly referenced work item into the walk's
// starting node (identity and parent hint only; a root's own content is
// already in the issue context). ReadWorkItemParents reads each child's immediate parent,
// returning one result per child in the children's order; a parent that
// cannot be read is an Omission, never an error. The error return is
// reserved for a cancelled context.
type WorkItemParentReader interface {
	AncestryRoot(repo RepositoryRef, item WorkItem) WorkItemNode
	ReadWorkItemParents(ctx context.Context, repo RepositoryRef, children []WorkItemNode, fields []string) ([]WorkItemParentRead, error)
}

// AncestryOptions bounds one walk.
type AncestryOptions struct {
	MaxDepth      int
	MaxItems      int
	IncludeTypes  []string
	CrossProject  string
	Fields        []string
	MaxFieldBytes int
}

// WorkItemAncestor is one included parent: the node, its distance from the
// directly referenced item, and the qualified keys of the children it parents.
type WorkItemAncestor struct {
	WorkItemNode
	Depth    int
	ParentOf []string
}

// AncestryOmission records one parent the walk did not include.
type AncestryOmission struct {
	Child  string
	Parent string
	Depth  int
	Reason string
	Detail string
}

// WorkItemAncestry is a walk's result, in deterministic order.
type WorkItemAncestry struct {
	Status    string
	Items     []WorkItemAncestor
	Omissions []AncestryOmission
}

// UnsupportedWorkItemAncestry is the explicit result for a provider with no
// parent relation reader.
func UnsupportedWorkItemAncestry() WorkItemAncestry {
	return WorkItemAncestry{Status: AncestryUnsupported, Items: []WorkItemAncestor{}, Omissions: []AncestryOmission{}}
}

// ancestryChain is one frontier entry: a node and the keys of its own chain,
// from the directly referenced item up to and including the node.
type ancestryChain struct {
	node    WorkItemNode
	lineage map[string]bool
}

type ancestryWalk struct {
	reader  WorkItemParentReader
	repo    RepositoryRef
	opts    AncestryOptions
	include map[string]bool
	items   map[string]*WorkItemAncestor
	omitted []AncestryOmission
	read    int
}

// TraverseWorkItemAncestry walks the parents of roots level by level, one
// provider read per level, until the chains end or a bound stops them.
// MaxDepth and MaxItems are enforced independently: a parent beyond MaxDepth
// is never read, and no more than MaxItems parents are ever read.
func TraverseWorkItemAncestry(ctx context.Context, reader WorkItemParentReader, repo RepositoryRef, roots []WorkItemNode, opts AncestryOptions) (WorkItemAncestry, error) {
	walk := &ancestryWalk{reader: reader, repo: repo, opts: opts, include: foldedSet(opts.IncludeTypes), items: map[string]*WorkItemAncestor{}}
	frontier := make([]ancestryChain, 0, len(roots))
	for _, root := range roots {
		frontier = append(frontier, ancestryChain{node: root, lineage: map[string]bool{root.Key(): true}})
	}
	for depth := 1; len(frontier) > 0; depth++ {
		next, err := walk.level(ctx, frontier, depth)
		if err != nil {
			return WorkItemAncestry{}, err
		}
		frontier = next
	}
	return walk.result(), nil
}

// level reads the parents of one frontier and returns the next frontier.
func (w *ancestryWalk) level(ctx context.Context, frontier []ancestryChain, depth int) ([]ancestryChain, error) {
	pending := w.pendingChains(frontier, depth)
	if len(pending) == 0 {
		return nil, nil
	}
	budget := max(w.opts.MaxItems-w.read, 0)
	for _, chain := range pending[min(budget, len(pending)):] {
		w.omit(chain.node, chain.node.ParentID, depth, AncestryOmitMaxItems, "")
	}
	pending = pending[:min(budget, len(pending))]
	if len(pending) == 0 {
		return nil, nil
	}
	children := make([]WorkItemNode, len(pending))
	for i, chain := range pending {
		children[i] = chain.node
	}
	reads, err := w.reader.ReadWorkItemParents(ctx, w.repo, children, w.opts.Fields)
	if err != nil {
		return nil, err
	}
	var next []ancestryChain
	for i, chain := range pending {
		if i >= len(reads) {
			w.omit(chain.node, chain.node.ParentID, depth, AncestryOmitReadFailed, "provider returned no result")
			continue
		}
		if follow, ok := w.accept(chain, reads[i], depth); ok {
			next = append(next, follow)
		}
	}
	return next, nil
}

// pendingChains filters a frontier to the chains whose parent should be read
// at depth, recording the depth bound, known cycles, and parents another
// chain already included.
func (w *ancestryWalk) pendingChains(frontier []ancestryChain, depth int) []ancestryChain {
	var pending []ancestryChain
	for _, chain := range frontier {
		if !chain.node.mayHaveParent() {
			continue
		}
		if depth > w.opts.MaxDepth {
			w.omit(chain.node, chain.node.ParentID, depth, AncestryOmitMaxDepth, "")
			continue
		}
		if chain.node.ParentID != "" {
			key := WorkItemNode{Provider: chain.node.Provider, Project: chain.node.Project, ID: chain.node.ParentID}.Key()
			if chain.lineage[key] {
				w.omit(chain.node, chain.node.ParentID, depth, AncestryOmitCycle, "")
				continue
			}
			if w.linkExisting(chain.node, key) {
				continue
			}
		}
		pending = append(pending, chain)
	}
	return pending
}

// accept applies one parent read to its chain, returning the chain to follow
// upward when there is one.
func (w *ancestryWalk) accept(chain ancestryChain, read WorkItemParentRead, depth int) (ancestryChain, bool) {
	child := chain.node
	if read.Omission != "" {
		w.omit(child, firstNonEmpty(read.ParentID, child.ParentID), depth, read.Omission, read.Detail)
		return ancestryChain{}, false
	}
	if read.Parent == nil {
		return ancestryChain{}, false
	}
	parent := *read.Parent
	w.read++
	key := parent.Key()
	switch {
	case !strings.EqualFold(parent.Project, child.Project) && !strings.EqualFold(w.opts.CrossProject, AncestryCrossProjectAllow):
		w.omit(child, key, depth, AncestryOmitCrossProject, "parent is in "+parent.Project)
		return ancestryChain{}, false
	case chain.lineage[key]:
		w.omit(child, key, depth, AncestryOmitCycle, "")
		return ancestryChain{}, false
	case w.linkExisting(child, key):
		return ancestryChain{}, false
	}
	if len(w.include) > 0 && !w.include[strings.ToLower(parent.Type)] {
		w.omit(child, key, depth, AncestryOmitExcludedType, parent.Type)
	} else {
		parent.Fields = boundFields(parent.Fields, w.opts.MaxFieldBytes)
		w.items[key] = &WorkItemAncestor{WorkItemNode: parent, Depth: depth, ParentOf: []string{child.Key()}}
	}
	return ancestryChain{node: parent, lineage: withKey(chain.lineage, key)}, true
}

// linkExisting records child under an ancestor another chain already
// included, reporting whether one was found.
func (w *ancestryWalk) linkExisting(child WorkItemNode, key string) bool {
	existing, ok := w.items[key]
	if !ok {
		return false
	}
	if !slices.Contains(existing.ParentOf, child.Key()) {
		existing.ParentOf = append(existing.ParentOf, child.Key())
	}
	return true
}

func (w *ancestryWalk) omit(child WorkItemNode, parent string, depth int, reason, detail string) {
	w.omitted = append(w.omitted, AncestryOmission{Child: child.Key(), Parent: parent, Depth: depth, Reason: reason, Detail: detail})
}

// result orders the walk deterministically: items by depth then key,
// omissions by depth, child, then reason.
func (w *ancestryWalk) result() WorkItemAncestry {
	out := WorkItemAncestry{Status: AncestryComplete, Items: make([]WorkItemAncestor, 0, len(w.items)), Omissions: w.omitted}
	for _, item := range w.items {
		sort.Strings(item.ParentOf)
		out.Items = append(out.Items, *item)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].Depth != out.Items[j].Depth {
			return out.Items[i].Depth < out.Items[j].Depth
		}
		return out.Items[i].Key() < out.Items[j].Key()
	})
	if out.Omissions == nil {
		out.Omissions = []AncestryOmission{}
	}
	sort.SliceStable(out.Omissions, func(i, j int) bool {
		a, b := out.Omissions[i], out.Omissions[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if a.Child != b.Child {
			return a.Child < b.Child
		}
		return a.Reason < b.Reason
	})
	for _, omission := range out.Omissions {
		if omission.Reason != AncestryOmitExcludedType {
			out.Status = AncestryIncomplete
		}
	}
	return out
}

// AncestryReadOmission classifies a failed parent read.
func AncestryReadOmission(err error) (string, string) {
	switch {
	case IsNotFoundError(err):
		return AncestryOmitNotFound, ""
	case IsAuthenticationError(err):
		return AncestryOmitAccessDenied, ""
	default:
		detail := err.Error()
		if len(detail) > ancestryMaxDetailBytes {
			detail = strings.ToValidUTF8(detail[:ancestryMaxDetailBytes], "")
		}
		return AncestryOmitReadFailed, detail
	}
}

// ancestryMaxDetailBytes bounds a failed read's detail in the artifact.
const ancestryMaxDetailBytes = 240

// ancestryContextError is the one error a parent reader returns: the walk's
// context ending. Every other failure becomes an omission.
func ancestryContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		return ctxErr
	}
	return nil
}

// boundFields truncates each field value to at most maxBytes bytes on a
// UTF-8 boundary, marking the ones it cut. maxBytes <= 0 leaves them whole.
func boundFields(fields []WorkItemField, maxBytes int) []WorkItemField {
	if maxBytes <= 0 {
		return fields
	}
	out := make([]WorkItemField, len(fields))
	for i, field := range fields {
		out[i] = field
		if len(field.Value) <= maxBytes {
			continue
		}
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(field.Value[cut]) {
			cut--
		}
		out[i].Value = field.Value[:cut]
		out[i].Truncated = true
	}
	return out
}

// selectAncestryFields returns the requested fields in request order, or the
// defaults when none are requested, skipping any the lookup has no value for.
func selectAncestryFields(requested, defaults []string, lookup func(string) string) []WorkItemField {
	names := requested
	if len(names) == 0 {
		names = defaults
	}
	fields := make([]WorkItemField, 0, len(names))
	for _, name := range names {
		if value := lookup(name); value != "" {
			fields = append(fields, WorkItemField{Name: name, Value: value})
		}
	}
	return fields
}

func foldedSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			set[strings.ToLower(trimmed)] = true
		}
	}
	return set
}

func withKey(lineage map[string]bool, key string) map[string]bool {
	out := make(map[string]bool, len(lineage)+1)
	for existing := range lineage {
		out[existing] = true
	}
	out[key] = true
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
