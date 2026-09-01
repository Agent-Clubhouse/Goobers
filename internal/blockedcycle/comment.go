package blockedcycle

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/providers"
)

// MaxCommentLength bounds every rendered comment so a provider never rejects
// an escalation for length.
const MaxCommentLength = 2000

const pathSeparator = " -> "

// Comments renders the provider comments that escalate a cycle: the cycle
// report itself, followed by the affected-issue list, split across additional
// comments when it does not fit alongside the report.
func Comments(cycle Result) []string {
	report := Comment(cycle.Paths, cycle.MorePaths)
	itemIDs := make([]string, len(cycle.Affected))
	for i, item := range cycle.Affected {
		itemIDs[i] = item.ItemID
	}

	memberList := " Affected issues: " + issueRefList(itemIDs) + "."
	if len(report)+len(memberList) <= MaxCommentLength {
		return []string{report + memberList}
	}

	comments := []string{report}
	const prefix = "Affected issues in this dependency cycle: "
	var current strings.Builder
	current.WriteString(prefix)
	for _, itemID := range itemIDs {
		separator := ""
		if current.Len() > len(prefix) {
			separator = ", "
		}
		reference := "#" + itemID
		if current.Len()+len(separator)+len(reference)+1 > MaxCommentLength {
			current.WriteByte('.')
			comments = append(comments, current.String())
			current.Reset()
			current.WriteString(prefix)
			separator = ""
		}
		current.WriteString(separator)
		current.WriteString(reference)
	}
	if current.Len() > len(prefix) {
		current.WriteByte('.')
		comments = append(comments, current.String())
	}
	return comments
}

// Comment renders the cycle report for the representative paths, truncating
// paths and omitting members as needed to stay within MaxCommentLength.
func Comment(paths [][]string, morePaths bool) string {
	const prefix = "Goobers detected circular issue dependencies. Representative cycles: "
	const additionalPathsOmitted = "additional cycle paths omitted"
	suffix := fmt.Sprintf(
		". Every issue in the cycle has been marked `%s` and removed from `%s` for human resolution.",
		providers.LabelNeedsHuman, providers.LabelReady,
	)
	available := MaxCommentLength - len(prefix) - len(suffix)
	if summaries, ok := completeSummaries(paths, morePaths, available, additionalPathsOmitted); ok {
		return prefix + summaries + suffix
	}

	var summaries strings.Builder
	included := 0
	for i, path := range paths {
		separatorLength := 0
		if summaries.Len() > 0 {
			separatorLength = 2
		}

		reservedNoticeLength := 0
		if morePaths || i < len(paths)-1 {
			reservedNoticeLength = 2 + len(additionalPathsOmitted)
		}
		pathBudget := available - summaries.Len() - separatorLength - reservedNoticeLength
		summary, truncated := boundedPath(path, pathBudget)
		if summary == "" {
			break
		}
		if separatorLength > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(summary)
		included++
		if truncated {
			break
		}
	}

	if morePaths || included < len(paths) {
		if summaries.Len() > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(additionalPathsOmitted)
	}
	return prefix + summaries.String() + suffix
}

// issueRefList renders issue numbers as "#441, #442" for provider comments.
func issueRefList(numbers []string) string {
	out := make([]byte, 0, len(numbers)*6)
	for i, n := range numbers {
		if i > 0 {
			out = append(out, ", "...)
		}
		out = append(out, '#')
		out = append(out, n...)
	}
	return string(out)
}

func renderPath(numbers []string) string {
	var out strings.Builder
	for i, n := range numbers {
		if i > 0 {
			out.WriteString(pathSeparator)
		}
		out.WriteByte('#')
		out.WriteString(n)
	}
	return out.String()
}

func pathLength(numbers []string, maxLength int) (int, bool) {
	length := 0
	for i, number := range numbers {
		addition := 1 + len(number)
		if i > 0 {
			addition += len(pathSeparator)
		}
		if addition > maxLength-length {
			return 0, false
		}
		length += addition
	}
	return length, true
}

func boundedPath(numbers []string, maxLength int) (string, bool) {
	if _, fits := pathLength(numbers, maxLength); fits {
		return renderPath(numbers), false
	}
	return truncatedPath(numbers, maxLength), true
}

func truncatedPath(numbers []string, maxLength int) string {
	if len(numbers) == 0 || maxLength <= 0 {
		return ""
	}

	bestHead, bestIdentified := 0, -1
	bestTail := false
	prefixLength := 0
	for head := 0; head < len(numbers); head++ {
		consider := func(includeTail bool) {
			omitted := len(numbers) - head
			identified := head
			if includeTail {
				omitted--
				identified++
			}
			if omitted <= 0 {
				return
			}

			length := prefixLength
			if head > 0 {
				length += len(pathSeparator)
			}
			length += len(membersOmitted(omitted))
			if includeTail {
				length += len(pathSeparator) + 1 + len(numbers[len(numbers)-1])
			}
			if length <= maxLength &&
				(identified > bestIdentified || identified == bestIdentified && head > bestHead) {
				bestHead = head
				bestTail = includeTail
				bestIdentified = identified
			}
		}

		consider(false)
		consider(head < len(numbers)-1)

		addition := 1 + len(numbers[head])
		if head > 0 {
			addition += len(pathSeparator)
		}
		prefixLength += addition
		if prefixLength > maxLength {
			break
		}
	}
	if bestIdentified < 0 {
		return ""
	}

	omitted := len(numbers) - bestHead
	if bestTail {
		omitted--
	}
	parts := make([]string, 0, bestHead+2)
	for _, number := range numbers[:bestHead] {
		parts = append(parts, "#"+number)
	}
	parts = append(parts, membersOmitted(omitted))
	if bestTail {
		parts = append(parts, "#"+numbers[len(numbers)-1])
	}
	return strings.Join(parts, pathSeparator)
}

func membersOmitted(count int) string {
	return fmt.Sprintf("[%d cycle members omitted]", count)
}

func completeSummaries(paths [][]string, morePaths bool, maxLength int, additionalPathsOmitted string) (string, bool) {
	total := 0
	for i, path := range paths {
		separatorLength := 0
		if i > 0 {
			separatorLength = 2
		}
		length, fits := pathLength(path, maxLength-total-separatorLength)
		if !fits {
			return "", false
		}
		total += separatorLength + length
	}
	if morePaths {
		separatorLength := 0
		if len(paths) > 0 {
			separatorLength = 2
		}
		if len(additionalPathsOmitted) > maxLength-total-separatorLength {
			return "", false
		}
		total += separatorLength + len(additionalPathsOmitted)
	}

	var summaries strings.Builder
	summaries.Grow(total)
	for i, path := range paths {
		if i > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(renderPath(path))
	}
	if morePaths {
		if summaries.Len() > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(additionalPathsOmitted)
	}
	return summaries.String(), true
}
