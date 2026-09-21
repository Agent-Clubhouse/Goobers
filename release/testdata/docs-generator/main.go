package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "__generate-docs" {
		fmt.Fprintln(os.Stderr, "usage: docs-generator __generate-docs <docs-directory>")
		os.Exit(2)
	}
	docsDir := os.Args[2]
	files := map[string]string{
		"cli/README.md":           "# release docs generator fixture\n",
		"completion/goobers.bash": "# release docs generator fixture\n",
		"completion/goobers.fish": "# release docs generator fixture\n",
		"completion/_goobers":     "# release docs generator fixture\n",
		"man/goobers.1":           ".TH GOOBERS 1\n\\\" release docs generator fixture\n",
	}
	for rel, content := range files {
		path := filepath.Join(docsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
