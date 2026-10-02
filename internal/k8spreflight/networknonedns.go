package k8spreflight

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/goobers/goobers/internal/runnercap"
)

// checkNetworkNoneDNS inspects rendered policy intent, never dataplane proof.
// NetworkPolicy grants are additive; an unrelated policy can still grant DNS.
func checkNetworkNoneDNS(ctx context.Context, client kubernetes.Interface, opts Options) Result {
	result := Result{ID: "network-none-dns", Title: "network:none DNS denial (configuration only)", Citation: "D12 / #5285", Severity: SeverityOptional, Status: StatusWarn}
	probeCtx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	policies, err := client.NetworkingV1().NetworkPolicies("").List(probeCtx, metav1.ListOptions{})
	if err != nil {
		result.Detail = "UNVERIFIED: cannot read policies: " + err.Error()
		return result
	}
	var details []string
	for _, policy := range policies.Items {
		if !slices.Contains(strings.Split(policy.Annotations[runnercap.AnnotationRunnerClassRestrictions], ","), string(runnercap.RestrictionNetworkNone)) {
			continue
		}
		dns := false
		for _, rule := range policy.Spec.Egress {
			if len(rule.Ports) == 0 {
				dns = true
			}
			for _, port := range rule.Ports {
				// Named ports need live endpoint resolution, so remain conservatively unproven.
				if port.Port == nil || port.Port.StrVal != "" || (port.Port.IntVal <= 53 && (port.EndPort != nil && *port.EndPort >= 53)) || port.Port.IntVal == 53 {
					dns = true
				}
			}
		}
		verdict := "no explicit DNS grant in class policy"
		if dns {
			verdict = "policy may permit DNS (including the deprecated migration escape)"
		}
		details = append(details, fmt.Sprintf("%s/%s: %s", policy.Namespace, policy.Name, verdict))
	}
	slices.Sort(details)
	if len(details) == 0 {
		details = append(details, "no network:none class policies found")
	}
	result.Detail = "UNVERIFIED dataplane: " + strings.Join(details, "; ")
	result.Hint = "From the selected network:none stage pod, verify daemon HTTPS by its original name and DNS timeout; from an unrestricted control pod verify the same DNS server is reachable. Also run the D12 denied-IP/allowed-IP controls. Configuration and a bare timeout cannot prove enforcement."
	return result
}
