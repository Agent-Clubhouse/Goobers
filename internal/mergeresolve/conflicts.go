package mergeresolve

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Git runs one git subcommand in the working tree being resolved and returns
// its raw stdout. Both consumers already own a hardened git runner (the
// rebase-pr CLI's exec calls, internal/worktree's context-aware rawGitOutput),
// so this package takes one rather than spawning processes of its own.
type Git func(args ...string) ([]byte, error)

// Status reports what a resolution pass found.
type Status uint8

const (
	// StatusAbsent means there was nothing unmerged to resolve.
	StatusAbsent Status = iota
	// StatusUnsafe means at least one unmerged path is outside the provably
	// safe class — the caller must treat the conflict as unresolved.
	StatusUnsafe
	// StatusResolved means every unmerged path was resolved and staged.
	StatusResolved
)

// Stage is one side of an unmerged index entry.
type Stage struct {
	Mode string
	OID  string
}

// File is one unmerged path with its ancestor (1), current-branch (2) and
// incoming (3) index stages.
type File struct {
	Path   string
	Stages map[int]Stage
}

// UnmergedFiles lists the working tree's unmerged index entries.
func UnmergedFiles(git Git) ([]File, error) {
	out, err := git("ls-files", "--unmerged", "-z")
	if err != nil {
		return nil, fmt.Errorf("list unmerged paths: %w", err)
	}

	var files []File
	byPath := make(map[string]int)
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, pathBytes, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("parse unmerged index entry %q", record)
		}
		fields := strings.Fields(string(header))
		if len(fields) != 3 {
			return nil, fmt.Errorf("parse unmerged index header %q", header)
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil || stage < 1 || stage > 3 {
			return nil, fmt.Errorf("parse unmerged index stage %q", fields[2])
		}
		path := string(pathBytes)
		index, ok := byPath[path]
		if !ok {
			index = len(files)
			byPath[path] = index
			files = append(files, File{Path: path, Stages: make(map[int]Stage, 3)})
		}
		if _, duplicate := files[index].Stages[stage]; duplicate {
			return nil, fmt.Errorf("duplicate unmerged index stage %d for %q", stage, path)
		}
		files[index].Stages[stage] = Stage{Mode: fields[0], OID: fields[1]}
	}
	return files, nil
}

// HasStandardTextMergeAttributes reports whether path carries no gitattributes
// that would make a line-level merge unsound (binary content, a custom merge
// driver, a clean/smudge filter, keyword expansion, a re-encoding).
func HasStandardTextMergeAttributes(git Git, path string) (bool, error) {
	out, err := git(
		"check-attr", "-z",
		"binary", "text", "diff", "merge",
		"filter", "ident", "working-tree-encoding",
		"--", path,
	)
	if err != nil {
		return false, err
	}
	parts := bytes.Split(out, []byte{0})
	if len(parts) == 0 || len(parts[len(parts)-1]) != 0 {
		return false, fmt.Errorf("malformed git check-attr output")
	}
	parts = parts[:len(parts)-1]
	if len(parts)%3 != 0 {
		return false, fmt.Errorf("malformed git check-attr output")
	}

	values := make(map[string]string, 4)
	for i := 0; i < len(parts); i += 3 {
		if string(parts[i]) != path {
			return false, fmt.Errorf("unexpected path %q in git check-attr output", parts[i])
		}
		values[string(parts[i+1])] = string(parts[i+2])
	}
	if values["binary"] != "unspecified" && values["binary"] != "unset" {
		return false, nil
	}
	switch values["text"] {
	case "unspecified", "set", "auto":
	default:
		return false, nil
	}
	switch values["diff"] {
	case "unspecified", "set":
	default:
		return false, nil
	}
	switch values["merge"] {
	case "unspecified", "set", "text":
	default:
		return false, nil
	}
	for _, attribute := range []string{"filter", "ident", "working-tree-encoding"} {
		if values[attribute] != "unspecified" && values[attribute] != "unset" {
			return false, nil
		}
	}
	return true, nil
}

// ResolveAdjacentLineConflicts resolves every unmerged path in the working
// tree at dir that is an adjacent-line insertion on both sides, writing the
// merged content and staging it. It inspects Git's three index stages rather
// than conflict-marker text so repository merge-marker configuration cannot
// widen what is considered safe, and it stages nothing at all unless every
// unmerged path qualifies.
func ResolveAdjacentLineConflicts(dir string, git Git) (Status, error) {
	files, err := UnmergedFiles(git)
	if err != nil {
		return StatusAbsent, err
	}
	if len(files) == 0 {
		return StatusAbsent, nil
	}

	type resolution struct {
		path string
		data []byte
	}
	resolutions := make([]resolution, 0, len(files))
	for _, file := range files {
		ancestor, hasAncestor := file.Stages[1]
		upstream, hasUpstream := file.Stages[2]
		incoming, hasIncoming := file.Stages[3]
		if !hasAncestor || !hasUpstream || !hasIncoming ||
			ancestor.Mode != upstream.Mode || ancestor.Mode != incoming.Mode ||
			(ancestor.Mode != "100644" && ancestor.Mode != "100755") {
			return StatusUnsafe, nil
		}
		standardText, err := HasStandardTextMergeAttributes(git, file.Path)
		if err != nil {
			return StatusUnsafe, fmt.Errorf("check merge attributes for %q: %w", file.Path, err)
		}
		if !standardText {
			return StatusUnsafe, nil
		}

		ancestorData, err := readBlob(git, ancestor.OID)
		if err != nil {
			return StatusUnsafe, fmt.Errorf("read ancestor for %q: %w", file.Path, err)
		}
		upstreamData, err := readBlob(git, upstream.OID)
		if err != nil {
			return StatusUnsafe, fmt.Errorf("read current version for %q: %w", file.Path, err)
		}
		incomingData, err := readBlob(git, incoming.OID)
		if err != nil {
			return StatusUnsafe, fmt.Errorf("read incoming version for %q: %w", file.Path, err)
		}
		merged, ok := MergeAdjacentLineInsertions(file.Path, ancestorData, upstreamData, incomingData)
		if !ok {
			return StatusUnsafe, nil
		}
		resolutions = append(resolutions, resolution{path: file.Path, data: merged})
	}

	for _, res := range resolutions {
		path, err := worktreePath(dir, res.path)
		if err != nil {
			return StatusUnsafe, err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return StatusUnsafe, fmt.Errorf("open conflict path %q: %w", res.path, err)
		}
		if _, err := file.Write(res.data); err != nil {
			_ = file.Close()
			return StatusUnsafe, fmt.Errorf("write conflict path %q: %w", res.path, err)
		}
		if err := file.Close(); err != nil {
			return StatusUnsafe, fmt.Errorf("close conflict path %q: %w", res.path, err)
		}
		if _, err := git("--literal-pathspecs", "add", "--", res.path); err != nil {
			return StatusUnsafe, fmt.Errorf("stage resolved path %q: %w", res.path, err)
		}
	}
	remaining, err := UnmergedFiles(git)
	if err != nil {
		return StatusUnsafe, err
	}
	if len(remaining) != 0 {
		return StatusUnsafe, fmt.Errorf("stage resolved conflicts: %d paths remain unmerged", len(remaining))
	}
	return StatusResolved, nil
}

func readBlob(git Git, oid string) ([]byte, error) {
	return git("cat-file", "blob", oid)
}

func worktreePath(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe conflict path %q", name)
	}
	return filepath.Join(dir, clean), nil
}
