package main

import "github.com/goobers/goobers/internal/instanceannotations"

func annotationRepositories(records map[string]instanceannotations.ItemRepository) map[string]recordedItemRepo {
	if records == nil {
		return nil
	}
	converted := make(map[string]recordedItemRepo, len(records))
	for id, record := range records {
		converted[id] = recordedItemRepo{repo: record.Repository, kind: record.Kind, purpose: record.Purpose}
	}
	return converted
}
