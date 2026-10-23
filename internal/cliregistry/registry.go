// Package cliregistry provides pure command-tree matching and completion.
package cliregistry

import (
	"fmt"
	"sort"
	"strings"
)

// Match returns the longest command path that prefixes args.
func Match(names, args []string) (index, consumed int, ok bool) {
	index = -1
	for i, name := range names {
		parts := strings.Fields(name)
		if len(parts) <= consumed || len(parts) > len(args) {
			continue
		}
		matched := true
		for n := range parts {
			matched = matched && parts[n] == args[n]
		}
		if matched {
			index, consumed = i, len(parts)
		}
	}
	return index, consumed, index >= 0
}

// Completion renders first-level command completion for a shell.
func Completion(binary, shell string, paths []string) string {
	seen := make(map[string]struct{}, len(paths))
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		name := strings.Fields(path)[0]
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	joined := strings.Join(names, " ")
	switch shell {
	case "bash":
		return fmt.Sprintf("complete -W %q %s\n", joined, binary)
	case "fish":
		var output strings.Builder
		for _, name := range names {
			fmt.Fprintf(&output, "complete -c %s -f -a %q\n", binary, name)
		}
		return output.String()
	default:
		return fmt.Sprintf("#compdef %s\n_arguments '1:command:(%s)'\n", binary, joined)
	}
}
