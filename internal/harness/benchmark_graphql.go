package harness

import (
	"encoding/json"
	"strings"

	"github.com/goobers/goobers/providers"
)

// benchmarkGraphQLMergeMutations are the GitHub GraphQL mutations that merge,
// queue, or unqueue a pull request. They are merges, exactly like the REST
// merge endpoint.
var benchmarkGraphQLMergeMutations = map[string]bool{
	"enqueuePullRequest":         true,
	"dequeuePullRequest":         true,
	"mergePullRequest":           true,
	"enablePullRequestAutoMerge": true,
	"mergeBranch":                true,
}

// classifyBenchmarkGraphQL classifies a POST /graphql request from its
// document, using the provider's own query/mutation distinction. A body that
// is not a GraphQL request is treated as an unidentifiable write, so no
// exchange can serve it.
func classifyBenchmarkGraphQL(body []byte) benchmarkOperation {
	var request struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Query) == "" {
		return benchmarkOperation{effect: BenchmarkEffectProviderWrite, graphQL: true}
	}
	if !providers.IsGraphQLMutation(request.Query) {
		return benchmarkOperation{effect: BenchmarkEffectProviderRead, graphQL: true}
	}
	op := benchmarkOperation{effect: BenchmarkEffectProviderWrite, graphQL: true}
	if fields := graphQLTopLevelFields(request.Query); len(fields) == 1 {
		op.mutation = fields[0]
	}
	switch {
	case benchmarkGraphQLMergeMutations[op.mutation]:
		op.effect = BenchmarkEffectMerge
	case op.mutation == "addComment":
		op.effect = BenchmarkEffectComment
	}
	return op
}

// graphQLTopLevelFields returns the field names (aliases resolved) selected
// directly in a GraphQL document's first operation selection set. It returns
// nil for a document whose selection set never closes.
func graphQLTopLevelFields(document string) []string {
	var fields []string
	depth, parens := 0, 0
	for i := 0; i < len(document); {
		c := document[i]
		switch {
		case c == '#':
			i = skipGraphQLComment(document, i)
			continue
		case c == '"':
			i = skipGraphQLString(document, i)
			continue
		case isGraphQLNameStart(c):
			end := i + 1
			for end < len(document) && isGraphQLNameChar(document[end]) {
				end++
			}
			if depth == 1 && parens == 0 {
				fields = append(fields, document[i:end])
			}
			i = end
			continue
		case c == '(':
			parens++
		case c == ')':
			parens--
		case c == ':' && depth == 1 && parens == 0 && len(fields) > 0:
			// The name before ':' was an alias; the field name follows it.
			fields = fields[:len(fields)-1]
		case c == '{' && parens == 0:
			depth++
		case c == '}' && parens == 0:
			depth--
			if depth == 0 {
				return fields
			}
		}
		i++
	}
	return nil
}

func skipGraphQLComment(document string, i int) int {
	if end := strings.IndexByte(document[i:], '\n'); end >= 0 {
		return i + end + 1
	}
	return len(document)
}

func skipGraphQLString(document string, i int) int {
	if strings.HasPrefix(document[i:], `"""`) {
		if end := strings.Index(document[i+3:], `"""`); end >= 0 {
			return i + 3 + end + 3
		}
		return len(document)
	}
	for j := i + 1; j < len(document); j++ {
		switch document[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(document)
}

func isGraphQLNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isGraphQLNameChar(c byte) bool {
	return isGraphQLNameStart(c) || (c >= '0' && c <= '9')
}
