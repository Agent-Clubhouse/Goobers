// Package dispatcher is the Goobernetes pod-per-stage substrate
// (docs/design/goobernetes-dispatcher.md, issue #3513): the resident
// goobers-system component that serves the (gaggle × runner-type) stage
// queues and, per stage attempt, creates ONE fresh pod for the resolved
// runner, supervises the stage, relays liveness to the live journal, confirms
// output surrender, and disposes the pod. A pod serves exactly one stage
// attempt, then is deleted — reuse is a correctness bug, not an optimization
// opportunity (decision record D1).
//
// The dispatcher is control plane, not workload (dispatcher §1, delivery
// decision 011): it is a DISTINCT deployment from the daemon with its own
// ServiceAccount and minimal RBAC, and it executes nothing itself. What it
// stamps on the pods it creates is the enforcement half of the restrictions
// model (restrictions doc D7): the pod-level securityContext and mount
// bindings, the derived non-overridable goobers.dev/runner-class label
// (delivery decisions 004/015 — internal/runnercap.RunnerClassValue is the
// single producer of the value), the deny-first posture labels the
// per-runner-class NetworkPolicies select on, and the always-on
// activeDeadlineSeconds orphan backstop (dispatcher §5).
//
// SCOPE SEAMS, stated plainly:
//
//   - Queue naming (QueueName) and the per-attempt Dispatch path live here;
//     binding a Temporal SDK worker onto those queues — re-pointing the
//     engine's stage activities from the resident tier-3 worker to this
//     package — is the #3482 migration cutover (architecture §12 open point
//     8), deliberately not taken in the same change that introduces the
//     substrate. Zero-declaration invariance (architecture §11.1) holds
//     trivially: nothing on the mode-1/2 path imports this package.
//   - The blob endpoint the stage pods fetch/put digests against (decision
//     010) may start daemon-fronted; BlobClient is the network client side of
//     that contract and the serving routes are a named follow-up.
//   - NetworkPolicy YAML is NEVER authored here (restrictions D7: the
//     operator holds no networking RBAC): the per-runner-class reference
//     manifests are rendered product artifacts selecting on the same
//     runnercap label constants and RunnerClassValue outputs this package
//     stamps (delivery decisions 015/016).
package dispatcher
