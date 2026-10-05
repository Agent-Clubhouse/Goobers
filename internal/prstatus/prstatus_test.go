package prstatus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goobers/goobers/providers"
)

type testPublisher struct {
	publish func(context.Context, providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error)
}

func (p testPublisher) PublishPullRequestStatus(
	ctx context.Context,
	request providers.PullRequestStatusRequest,
) (providers.PullRequestStatusResult, error) {
	return p.publish(ctx, request)
}

func TestParseState(t *testing.T) {
	tests := []struct {
		input string
		want  providers.CheckState
	}{
		{input: "succeeded", want: providers.CheckStatePassing},
		{input: "success", want: providers.CheckStatePassing},
		{input: "passing", want: providers.CheckStatePassing},
		{input: "failed", want: providers.CheckStateFailing},
		{input: "failure", want: providers.CheckStateFailing},
		{input: "failing", want: providers.CheckStateFailing},
		{input: "pending", want: providers.CheckStatePending},
		{input: "", want: providers.CheckStatePending},
	}
	for _, test := range tests {
		got, err := ParseState(test.input)
		if err != nil {
			t.Fatalf("ParseState(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Fatalf("ParseState(%q) = %q, want %q", test.input, got, test.want)
		}
	}

	got, err := ParseState("bogus")
	if got != "" || err == nil || err.Error() != `unknown status state "bogus" (want succeeded|failed|pending)` {
		t.Fatalf("ParseState(bogus) = %q, %v", got, err)
	}
}

func TestPublishForwardsRequestAndWritesExactResult(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "request-context")
	request := providers.PullRequestStatusRequest{
		Repository: providers.RepositoryRef{
			Provider: providers.ProviderADO,
			Owner:    "acme",
			Project:  "project",
			Name:     "repo",
		},
		PullID:      "77\"\n",
		Genre:       "quality<&",
		Name:        "review/\"",
		State:       providers.CheckStateFailing,
		Description: "review rejected",
		TargetURL:   "https://runs.example/123",
		HeadSHA:     "reviewed-head",
	}
	resultFile := filepath.Join(t.TempDir(), "status-result.json")
	calls := 0
	publisher := testPublisher{publish: func(gotCtx context.Context, gotRequest providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error) {
		calls++
		if gotCtx != ctx {
			t.Fatal("Publish did not forward the original context")
		}
		if !reflect.DeepEqual(gotRequest, request) {
			t.Fatalf("request = %#v, want %#v", gotRequest, request)
		}
		return providers.PullRequestStatusResult{ID: 31}, nil
	}}

	result, err := Publish(ctx, publisher, request, resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if result != (providers.PullRequestStatusResult{ID: 31}) {
		t.Fatalf("result = %#v, want id 31", result)
	}
	if calls != 1 {
		t.Fatalf("publication calls = %d, want 1", calls)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"prNumber":"77\"\n","state":"failing","statusGenre":"quality\u003c\u0026","statusId":"31","statusName":"review/\""}`
	if string(data) != want {
		t.Fatalf("result bytes = %s, want %s", data, want)
	}
}

func TestPublishReturnsOriginalPublisherErrors(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{name: "provider error", ctx: context.Background(), err: errors.New("provider unavailable")},
		{name: "cancellation", ctx: canceledContext(), err: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			publisher := testPublisher{publish: func(ctx context.Context, _ providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error) {
				calls++
				if errors.Is(test.err, context.Canceled) && !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatalf("context error = %v, want context canceled", ctx.Err())
				}
				return providers.PullRequestStatusResult{}, test.err
			}}

			_, err := Publish(test.ctx, publisher, providers.PullRequestStatusRequest{}, filepath.Join(t.TempDir(), "result.json"))
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want original error %v", err, test.err)
			}
			if calls != 1 {
				t.Fatalf("publication calls = %d, want 1", calls)
			}
		})
	}
}

func TestPublishFileFailureDoesNotRepublish(t *testing.T) {
	resultFile := filepath.Join(t.TempDir(), "result")
	if err := os.Mkdir(resultFile, 0o755); err != nil {
		t.Fatal(err)
	}
	calls := 0
	publisher := testPublisher{publish: func(context.Context, providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error) {
		calls++
		return providers.PullRequestStatusResult{ID: 12}, nil
	}}

	result, err := Publish(context.Background(), publisher, providers.PullRequestStatusRequest{}, resultFile)
	if result.ID != 12 {
		t.Fatalf("result id = %d, want 12", result.ID)
	}
	var writeErr *ResultWriteError
	if !errors.As(err, &writeErr) {
		t.Fatalf("error = %T %v, want ResultWriteError", err, err)
	}
	if calls != 1 {
		t.Fatalf("publication calls = %d, want exactly 1", calls)
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
