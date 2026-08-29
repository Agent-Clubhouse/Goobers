package dispatcher

import (
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestQueueNameKeysGaggleByRunnerType(t *testing.T) {
	a := QueueName("alpha", "tiny-linux")
	b := QueueName("beta", "tiny-linux")
	c := QueueName("alpha", "win-large")
	if a == b || a == c || b == c {
		t.Fatalf("queue names must be distinct per (gaggle × runner-type): %q %q %q", a, b, c)
	}
	if a != "goobers-dispatch.alpha.tiny-linux" {
		t.Fatalf("QueueName = %q", a)
	}
}

func TestQueuesExcludeSelfRunners(t *testing.T) {
	runners := []RunnerSpec{
		{Name: "self", HostKind: instance.RunnerHostSelf},
		{Name: "tiny-linux", HostKind: instance.RunnerHostImage},
		{Name: "win-large", HostKind: instance.RunnerHostImage},
	}
	got := Queues([]string{"beta", "alpha"}, runners)
	want := []string{
		"goobers-dispatch.alpha.tiny-linux",
		"goobers-dispatch.alpha.win-large",
		"goobers-dispatch.beta.tiny-linux",
		"goobers-dispatch.beta.win-large",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Queues = %v, want %v (sorted, self excluded)", got, want)
	}
}
