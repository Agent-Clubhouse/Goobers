package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestUnparkMatchingPRs(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	provider := &unparkSweepProvider{
		prs: []providers.PullRequestSummary{
			{Number: 10, Labels: []string{"parked"}},
			{Number: 11, Labels: []string{"parked"}},
			{Number: 12, Labels: []string{"other"}},
			{Number: 13, Labels: []string{"parked"}},
			{Number: 14, Labels: []string{"parked"}},
			{Number: 15, Labels: []string{"parked"}},
		},
		updateErrors: map[string]error{"14": errors.New("update failed")},
	}
	var checked []int
	sweep := unparkSweep{
		label:        "parked",
		removeLabels: []string{"parked"},
		addLabels:    []string{"ready"},
		listError: func(base string, err error) error {
			return fmt.Errorf("list %s: %w", base, err)
		},
		check: func(_ context.Context, _ remediationProvider, _ providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
			checked = append(checked, pr.Number)
			switch pr.Number {
			case 11:
				return true, nil
			case 13:
				return false, errors.New("check failed")
			default:
				return false, nil
			}
		},
		checkError: func(number int, err error) error {
			return fmt.Errorf("check %d: %w", number, err)
		},
		updateError: func(number int, err error) error {
			return fmt.Errorf("update %d: %w", number, err)
		},
	}

	unparked, errs := unparkMatchingPRs(context.Background(), provider, repo, 10, "main", sweep)

	if !reflect.DeepEqual(unparked, []int{15}) {
		t.Fatalf("unparked = %v, want [15]", unparked)
	}
	if got, want := errorStrings(errs), []string{"check 13: check failed", "update 14: update failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("errors = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(checked, []int{11, 13, 14, 15}) {
		t.Fatalf("checked = %v, want [11 13 14 15]", checked)
	}
	wantList := providers.ListPullRequestsRequest{
		Repository: repo, Base: "main", HeadPrefix: providerBranchNamespace(), SkipCheckState: true,
	}
	if !reflect.DeepEqual(provider.listRequest, wantList) {
		t.Fatalf("ListPullRequests request = %+v, want %+v", provider.listRequest, wantList)
	}
	wantUpdates := []providers.UpdateWorkItemRequest{
		{Repository: repo, ID: "14", RemoveLabels: []string{"parked"}, AddLabels: []string{"ready"}},
		{Repository: repo, ID: "15", RemoveLabels: []string{"parked"}, AddLabels: []string{"ready"}},
	}
	if !reflect.DeepEqual(provider.updates, wantUpdates) {
		t.Fatalf("updates = %+v, want %+v", provider.updates, wantUpdates)
	}
}

func TestUnparkMatchingPRsListGuardsAndErrors(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	listErr := errors.New("list failed")
	provider := &unparkSweepProvider{listError: listErr}
	sweep := unparkSweep{
		listError: func(base string, err error) error {
			return fmt.Errorf("list %s: %w", base, err)
		},
	}

	unparked, errs := unparkMatchingPRs(context.Background(), provider, repo, 10, "", sweep)
	if unparked != nil || errs != nil || provider.listCalls != 0 {
		t.Fatalf("empty base = (%v, %v), list calls %d; want (nil, nil), 0 calls", unparked, errs, provider.listCalls)
	}

	unparked, errs = unparkMatchingPRs(context.Background(), provider, repo, 10, "main", sweep)
	if unparked != nil {
		t.Fatalf("unparked = %v, want nil", unparked)
	}
	if got, want := errorStrings(errs), []string{"list main: list failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("errors = %v, want %v", got, want)
	}
}

func TestUnparkSweepContracts(t *testing.T) {
	tests := []struct {
		name         string
		sweep        unparkSweep
		removeLabels []string
		addLabels    []string
		listError    string
		checkError   string
		updateError  string
	}{
		{
			name:         "escalation",
			sweep:        selfHealedEscalationSweep(),
			removeLabels: []string{remediationEscalatedLabel},
			addLabels:    []string{needsRemediationLabel},
			listError:    "list open pull requests targeting main for merge-escalated unpark: failure",
			checkError:   "check merge-escalated state for pr #42 during unpark: failure",
			updateError:  "clear goobers:merge-escalated from pr #42: failure",
		},
		{
			name:         "demotion",
			sweep:        selfHealedDemotionSweep(),
			removeLabels: []string{mergeDemotedLabel},
			listError:    "list open pull requests targeting main for merge-demoted unpark: failure",
			checkError:   "check merge-demoted state for pr #42 during unpark: failure",
			updateError:  "clear goobers:merge-demoted from pr #42: failure",
		},
		{
			name:         "blocked sibling",
			sweep:        resolvedSiblingSweep(),
			removeLabels: []string{blockedOnSiblingLabel},
			listError:    "list open pull requests targeting main for blocked-on-sibling unpark: failure",
			checkError:   "check blocked-on-sibling state for pr #42 during unpark: failure",
			updateError:  "clear goobers:blocked-on-sibling from pr #42: failure",
		},
	}
	errFailure := errors.New("failure")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(tt.sweep.removeLabels, tt.removeLabels) {
				t.Errorf("remove labels = %v, want %v", tt.sweep.removeLabels, tt.removeLabels)
			}
			if !reflect.DeepEqual(tt.sweep.addLabels, tt.addLabels) {
				t.Errorf("add labels = %v, want %v", tt.sweep.addLabels, tt.addLabels)
			}
			if got := tt.sweep.listError("main", errFailure).Error(); got != tt.listError {
				t.Errorf("list error = %q, want %q", got, tt.listError)
			}
			if got := tt.sweep.checkError(42, errFailure).Error(); got != tt.checkError {
				t.Errorf("check error = %q, want %q", got, tt.checkError)
			}
			if got := tt.sweep.updateError(42, errFailure).Error(); got != tt.updateError {
				t.Errorf("update error = %q, want %q", got, tt.updateError)
			}
		})
	}
}

func errorStrings(errs []error) []string {
	out := make([]string, len(errs))
	for i, err := range errs {
		out[i] = err.Error()
	}
	return out
}

type unparkSweepProvider struct {
	remediationProvider
	prs          []providers.PullRequestSummary
	listRequest  providers.ListPullRequestsRequest
	listError    error
	listCalls    int
	updates      []providers.UpdateWorkItemRequest
	updateErrors map[string]error
}

func (p *unparkSweepProvider) ListPullRequests(_ context.Context, req providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error) {
	p.listCalls++
	p.listRequest = req
	return p.prs, p.listError
}

func (p *unparkSweepProvider) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	p.updates = append(p.updates, req)
	return providers.WorkItem{}, p.updateErrors[req.ID]
}
