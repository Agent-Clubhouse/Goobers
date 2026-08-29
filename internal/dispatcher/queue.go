package dispatcher

import (
	"fmt"
	"sort"

	"github.com/goobers/goobers/internal/instance"
)

// QueuePrefix roots every dispatcher stage queue name, so the (gaggle ×
// runner-type) queues are recognizable in Temporal visibility and cannot
// collide with the engine's workflow queues.
const QueuePrefix = "goobers-dispatch"

// QueueName derives the Temporal task queue for one (gaggle × runner-type)
// pair — decision record D9's fairness/isolation keying (#656's contract
// survives): per-gaggle fairness is a QUEUE property, not a process-location
// property (dispatcher §1), so ONE dispatcher polls every queue and fairness
// still holds. runnerName is the runners-inventory entry name (validated as a
// lowercase DNS label at config load) and gaggle the gaggle name; both are
// deterministic declared inputs, keeping placement a pure function (D8).
func QueueName(gaggle, runnerName string) string {
	return fmt.Sprintf("%s.%s.%s", QueuePrefix, gaggle, runnerName)
}

// Queues enumerates the queue set a dispatcher serves for an inventory:
// every declared gaggle crossed with every NON-self runner, sorted. Self
// runners never appear — the local execution path is the daemon's, not the
// dispatcher's (architecture §3).
func Queues(gaggles []string, runners []RunnerSpec) []string {
	var queues []string
	for _, gaggle := range gaggles {
		for _, runner := range runners {
			if runner.HostKind == instance.RunnerHostSelf {
				continue
			}
			queues = append(queues, QueueName(gaggle, runner.Name))
		}
	}
	sort.Strings(queues)
	return queues
}
