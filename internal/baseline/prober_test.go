package baseline

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubCheckout struct {
	dir      string
	err      error
	released int
}

func (c *stubCheckout) Materialize(context.Context, ProbeTarget) (string, func(), error) {
	if c.err != nil {
		return "", nil, c.err
	}
	return c.dir, func() { c.released++ }, nil
}

func TestCommandProberReportsAGreenBaseline(t *testing.T) {
	checkout := &stubCheckout{dir: "/checkout"}
	prober := &CommandProber{
		Checkout: checkout,
		Exec: func(_ context.Context, dir string, _, command []string) (string, bool, error) {
			if dir != "/checkout" {
				t.Fatalf("dir = %q, want the materialized base checkout", dir)
			}
			if CommandKey(command) != CommandKey([]string{"make", "ci"}) {
				t.Fatalf("command = %v, want the CI command under test", command)
			}
			return "ok", true, nil
		},
	}

	result, err := prober.Probe(context.Background(), ProbeTarget{Repo: "acme/web", BaseSHA: "sha-1"}, []string{"make", "ci"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !result.Green {
		t.Fatal("green = false, want true")
	}
	if checkout.released != 1 {
		t.Fatalf("released = %d, want the disposable checkout released exactly once", checkout.released)
	}
}

func TestCommandProberReportsARedBaseline(t *testing.T) {
	checkout := &stubCheckout{dir: "/checkout"}
	prober := &CommandProber{
		Checkout: checkout,
		Exec: func(context.Context, string, []string, []string) (string, bool, error) {
			return "--- FAIL: TestThing", false, nil
		},
	}

	result, err := prober.Probe(context.Background(), ProbeTarget{Repo: "acme/web", BaseSHA: "sha-1"}, []string{"make", "ci"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Green || !strings.Contains(result.Output, "--- FAIL") {
		t.Fatalf("result = %+v, want a red baseline carrying its failure output", result)
	}
	if checkout.released != 1 {
		t.Fatalf("released = %d, want the checkout released even for a red baseline", checkout.released)
	}
}

func TestCommandProberSurfacesSetupFailures(t *testing.T) {
	if _, err := (&CommandProber{}).Probe(context.Background(), ProbeTarget{Repo: "acme/web", BaseSHA: "sha"}, []string{"make"}); err == nil {
		t.Fatal("Probe error = nil, want an error without a checkout")
	}
	prober := &CommandProber{Checkout: &stubCheckout{dir: "/checkout"}}
	if _, err := prober.Probe(context.Background(), ProbeTarget{Repo: "acme/web", BaseSHA: "sha"}, nil); err == nil {
		t.Fatal("Probe error = nil, want an error without a command")
	}
	failing := &CommandProber{Checkout: &stubCheckout{err: errors.New("clone refused")}}
	if _, err := failing.Probe(context.Background(), ProbeTarget{Repo: "acme/web", BaseSHA: "sha"}, []string{"make"}); err == nil {
		t.Fatal("Probe error = nil, want the checkout failure surfaced: an unmeasurable baseline is not a green one")
	}
	unrunnable := &CommandProber{
		Checkout: &stubCheckout{dir: "/checkout"},
		Exec: func(context.Context, string, []string, []string) (string, bool, error) {
			return "", false, errors.New("executable not found")
		},
	}
	if _, err := unrunnable.Probe(context.Background(), ProbeTarget{Repo: "acme/web", BaseSHA: "sha"}, []string{"make"}); err == nil {
		t.Fatal("Probe error = nil, want a command that could not run reported as an error, not as a red baseline")
	}
}

func TestBoundOutputKeepsTheTail(t *testing.T) {
	head := strings.Repeat("noise\n", maxProbeOutput/6+100)
	got := boundOutput(head + "--- FAIL: TestTail\n")
	if len(got) > maxProbeOutput {
		t.Fatalf("output length = %d, want it bounded to %d", len(got), maxProbeOutput)
	}
	if !strings.Contains(got, "--- FAIL: TestTail") {
		t.Fatal("bounded output dropped the trailing failure summary")
	}
	if short := "already small"; boundOutput(short) != short {
		t.Fatal("bounded output altered an already-small output")
	}
}
