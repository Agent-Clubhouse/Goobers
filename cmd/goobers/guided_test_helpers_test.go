package main

import "github.com/goobers/goobers/internal/instance"

func seedGuidedSourceForTest(root string, opts instance.GuidedOptions) error {
	_, err := instance.SeedGuidedConfigSource(root, opts)
	return err
}
