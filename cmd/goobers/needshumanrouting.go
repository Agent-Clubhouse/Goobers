package main

import "github.com/goobers/goobers/providers"

func withNeedsHumanAssignee(req providers.UpdateWorkItemRequest, assignee string) providers.UpdateWorkItemRequest {
	if assignee == "" {
		return req
	}
	for _, label := range req.AddLabels {
		if label == providers.LabelNeedsHuman {
			req.Assignee = &assignee
			break
		}
	}
	return req
}
