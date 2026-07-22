package main

import "os"

// readDirectory distinguishes a missing path from an existing non-directory.
// On Windows, os.IsNotExist also matches ERROR_DIRECTORY, so checking only that
// predicate would silently treat a corrupt file-at-directory path as absent.
func readDirectory(path string) ([]os.DirEntry, bool, error) {
	entries, err := os.ReadDir(path)
	if err == nil {
		return entries, true, nil
	}
	if !os.IsNotExist(err) {
		return nil, true, err
	}
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return nil, false, nil
	}
	return nil, true, err
}
