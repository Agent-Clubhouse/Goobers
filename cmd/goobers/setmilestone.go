package main

import (
	"flag"
	"io"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

const setMilestoneHelp = "Usage: goobers set-milestone --item ID --milestone N [path]\n\n" +
	"Assign an existing GitHub milestone number to an issue. Task inputs itemID\n" +
	"and milestone provide the same workflow-stage configuration as the flags.\n" +
	"Requires the github:milestones:write capability; github:issues:write alone\n" +
	"does not authorize this command. Exit codes: 0 = assigned, 1 = business or\n" +
	"provider error, 2 = usage/IO error.\n"

func runSetMilestone(args []string, stdout, stderr io.Writer) int {
	milestoneDefault := 0
	if raw := providerInput("milestone", ""); raw != "" {
		number, err := strconv.Atoi(raw)
		if err != nil || number <= 0 {
			pf(stderr, "error: invalid milestone input %q (want a positive integer)\n", raw)
			return 1
		}
		milestoneDefault = number
	}

	fs := flag.NewFlagSet("set-milestone", flag.ContinueOnError)
	fs.SetOutput(stderr)
	itemID := fs.String("item", providerInput("itemID", ""), "issue identifier")
	milestone := fs.Int("milestone", milestoneDefault, "existing milestone number")
	fs.Usage = helpUsage(stderr, "set-milestone")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	*itemID = strings.TrimSpace(*itemID)
	if *itemID == "" {
		pln(stderr, "error: item is required")
		return 1
	}
	if *milestone <= 0 {
		pln(stderr, "error: milestone must be a positive integer")
		return 1
	}

	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubMilestonesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	provider := newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
	ctx, cancel := providerCommandContext()
	defer cancel()
	item, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         *itemID,
		Milestone:  milestone,
	})
	if err != nil {
		return failProviderStage(stderr, "set milestone", err, "milestone-result.json")
	}
	if err := writeProviderStageResult(providerInput("resultFile", "milestone-result.json"), map[string]interface{}{
		"itemId":    item.ID,
		"milestone": *milestone,
	}); err != nil {
		pf(stderr, "error: write milestone result: %v\n", err)
		return 2
	}
	pf(stdout, "assigned issue %s to milestone %d\n", item.ID, *milestone)
	return 0
}
