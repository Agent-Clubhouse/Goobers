package providers

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type inventoryDispatchProvider struct {
	Provider
	caps    CapabilitySet
	calls   int
	request MergeInventoryRequest
	err     error
}

func (p *inventoryDispatchProvider) Kind() ProviderKind          { return ProviderGitHub }
func (p *inventoryDispatchProvider) Capabilities() CapabilitySet { return p.caps }
func (p *inventoryDispatchProvider) MergeInventory(_ context.Context, request MergeInventoryRequest) ([]MergeInventoryEntry, error) {
	p.calls++
	p.request = request
	if p.err != nil {
		return nil, p.err
	}
	return []MergeInventoryEntry{{Provider: ProviderGitHub, PullID: "9"}}, nil
}

func TestMergeInventoryDispatcherRequiresDeclarationAndImplementation(t *testing.T) {
	undeclared := &inventoryDispatchProvider{}
	for _, p := range []Provider{undeclared, &fakeCapableProvider{caps: NewCapabilitySet(CapPRMergeInventory)}, &ADOProvider{}, &GiteaProvider{}} {
		got, err := NewDispatcher(p).MergeInventory(context.Background(), MergeInventoryRequest{})
		var unsupported ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Capability != CapPRMergeInventory || got != nil {
			t.Fatalf("%T inventory=%v err=%v", p, got, err)
		}
	}
	if undeclared.calls != 0 {
		t.Fatal("undeclared operation reached provider")
	}
}

func TestMergeInventoryDispatcherPreservesRequestAndError(t *testing.T) {
	p := &inventoryDispatchProvider{caps: NewCapabilitySet(CapPRMergeInventory)}
	d := NewDispatcher(p)
	request := MergeInventoryRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, Since: time.Now().UTC(), Until: time.Now().UTC().Add(time.Hour), Limit: 17}
	got, err := d.MergeInventory(context.Background(), request)
	if err != nil || len(got) != 1 || got[0].PullID != "9" || p.calls != 1 || !reflect.DeepEqual(p.request, request) {
		t.Fatalf("request=%+v got=%+v calls=%d err=%v", p.request, got, p.calls, err)
	}
	p.err = errors.New("forge unavailable")
	got, err = d.MergeInventory(context.Background(), request)
	if !errors.Is(err, p.err) || got != nil || p.calls != 2 {
		t.Fatalf("provider error hidden: got=%+v err=%v", got, err)
	}
}
