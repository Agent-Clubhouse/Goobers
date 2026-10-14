package workerrouting

import (
	"fmt"
	"strings"
	"time"

	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// version is one routing target or served version: a goobers build, or
// unversioned workers.
type version struct {
	versioned bool
	build     string
}

func versionOf(v *worker.WorkerDeploymentVersion) version {
	if v == nil {
		return version{}
	}
	return version{versioned: true, build: v.BuildID}
}

func (v version) label() string {
	if !v.versioned {
		return UnversionedLabel
	}
	return DeploymentName + "." + v.build
}

// promoteCommand makes v the deployment's current version, which also ends
// a ramp to it.
func (v version) promoteCommand() string {
	if !v.versioned {
		return UnversionedCommand()
	}
	return BuildCommand(v.build)
}

// rampDeleteCommand removes a ramp to v, returning its share to the current
// version.
func (v version) rampDeleteCommand() string {
	target := "--unversioned"
	if v.versioned {
		target = "--build-id " + v.build
	}
	return fmt.Sprintf("temporal worker deployment set-ramping-version --deployment-name %s %s --delete", DeploymentName, target)
}

type pollerGroup struct {
	version
	count int
}

// routeTarget is a version the deployment sends a share of new workflow
// tasks to, and whether a live worker serves it.
type routeTarget struct {
	version
	share float32
	live  bool
}

// trafficPlan is the deployment's routing as traffic shares: the current
// version receives 100 minus the ramp percentage, the ramping version the
// ramp percentage. A ramp with no version is a ramp to unversioned workers.
type trafficPlan struct {
	current routeTarget
	ramp    *routeTarget
}

func planTraffic(routing client.WorkerDeploymentRoutingConfig, groups []pollerGroup) trafficPlan {
	live := map[version]bool{}
	for _, group := range groups {
		live[group.version] = true
	}
	ramp := min(max(routing.RampingVersionPercentage, 0), 100)
	plan := trafficPlan{current: routeTarget{version: versionOf(routing.CurrentVersion), share: 100}}
	if routing.RampingVersion != nil || ramp > 0 {
		target := routeTarget{version: versionOf(routing.RampingVersion), share: ramp}
		target.live = live[target.version]
		plan.ramp = &target
		plan.current.share = 100 - ramp
	}
	plan.current.live = live[plan.current.version]
	return plan
}

func (p trafficPlan) targets() []routeTarget {
	if p.ramp == nil {
		return []routeTarget{p.current}
	}
	return []routeTarget{p.current, *p.ramp}
}

// shareOf is the percentage of new workflow tasks v receives.
func (p trafficPlan) shareOf(v version) float32 {
	var share float32
	for _, target := range p.targets() {
		if target.version == v {
			share += target.share
		}
	}
	return share
}

func (p trafficPlan) rampServed() bool {
	return p.ramp != nil && p.ramp.share > 0 && p.ramp.live
}

func (p trafficPlan) describe() string {
	text := fmt.Sprintf("current %s (%s)", p.current.label(), percent(p.current.share))
	if p.ramp != nil {
		text += fmt.Sprintf(", ramping %s (%s)", p.ramp.label(), percent(p.ramp.share))
	}
	return text
}

func percent(share float32) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", share), "0"), ".") + "%"
}

// evaluate checks both directions of the routing: every version that
// receives a share of new workflow tasks needs a live worker, and every live
// worker build should receive a share. No live worker for any share stalls
// the engine; anything less is degraded.
func evaluate(taskQueue string, routing client.WorkerDeploymentRoutingConfig, describeErr error, pollers []*taskqueuepb.PollerInfo, now time.Time) Assessment {
	groups := livePollerGroups(pollers, now)
	if len(groups) == 0 {
		return Unknown(taskQueue, fmt.Sprintf("no goobers worker has polled task queue %q for workflow tasks in the last %s, "+
			"so there is no live build to check the routing against", taskQueue, PollerMaxAge), now)
	}
	if describeErr != nil && !deploymentAbsent(describeErr) {
		return Unknown(taskQueue, fmt.Sprintf("cannot read worker deployment %q routing: %v", DeploymentName, describeErr), now)
	}
	a := Assessment{Deployment: DeploymentName, TaskQueue: taskQueue, CheckedAt: now.UTC()}
	if describeErr != nil {
		// No deployment: new tasks go to unversioned workers. A live
		// versioned worker means the deployment should exist, so its
		// routing is unknown rather than absent.
		if unverified := unverifiedWorkers(&a, groups); unverified != "" {
			a.State, a.Detail = StateUnknown, fmt.Sprintf("live workers serve %s, but worker deployment %q cannot be described: %v",
				unverified, DeploymentName, describeErr)
			return a
		}
	}
	plan := planTraffic(routing, groups)
	if describeErr == nil {
		a.CurrentVersion = plan.current.label()
		if plan.ramp != nil {
			a.RampingVersion, a.RampingPercentage = plan.ramp.label(), plan.ramp.share
		}
	}
	var unrouted []string
	for _, group := range groups {
		worker := WorkerRouting{Version: group.label(), Pollers: group.count, Routing: WorkerRouted, TrafficPercent: plan.shareOf(group.version)}
		if worker.TrafficPercent == 0 {
			worker.Routing = WorkerUnrouted
			unrouted = append(unrouted, worker.Version)
		}
		a.Workers = append(a.Workers, worker)
	}
	a.Remediation = remediation(plan, groups)
	a.State, a.Detail = verdict(a, plan, describeErr != nil, unrouted)
	return a
}

func unverifiedWorkers(a *Assessment, groups []pollerGroup) string {
	var versioned []string
	for _, group := range groups {
		routing := WorkerRouted
		if group.versioned {
			routing = WorkerUnknown
			versioned = append(versioned, group.label())
		}
		a.Workers = append(a.Workers, WorkerRouting{Version: group.label(), Pollers: group.count, Routing: routing, TrafficPercent: 0})
	}
	if len(versioned) == 0 {
		a.Workers = nil
	}
	return strings.Join(versioned, ", ")
}

func verdict(a Assessment, plan trafficPlan, absent bool, unrouted []string) (State, string) {
	var covered float32
	var problems []string
	for _, target := range plan.targets() {
		switch {
		case target.live:
			covered += target.share
		case target.share > 0:
			problems = append(problems, fmt.Sprintf("%s of new workflow tasks go to %s, which no live worker serves", percent(target.share), target.label()))
		}
	}
	if len(unrouted) > 0 {
		problems = append(problems, fmt.Sprintf("live workers serving %s receive no new workflow tasks%s", strings.Join(unrouted, ", "), unversionedNote(plan, unrouted)))
	}
	routes := plan.describe()
	if absent {
		routes = "current " + UnversionedLabel + " (100%; no versioned worker has registered the deployment)"
	}
	live := make([]string, 0, len(a.Workers))
	for _, worker := range a.Workers {
		live = append(live, worker.Version)
	}
	switch {
	case covered == 0:
		return StateStalled, fmt.Sprintf("worker deployment %q routes new workflow tasks on task queue %q as %s, and no live worker serves any of it: "+
			"%s, so the engine accepts no new work", a.Deployment, a.TaskQueue, routes, strings.Join(problems, "; "))
	case len(problems) > 0:
		return StateDegraded, fmt.Sprintf("worker deployment %q routes task queue %q as %s: %s",
			a.Deployment, a.TaskQueue, routes, strings.Join(problems, "; "))
	default:
		return StateHealthy, fmt.Sprintf("worker deployment %q routes workflow tasks to the live workers on task queue %q (%s); %s",
			a.Deployment, a.TaskQueue, strings.Join(live, ", "), routes)
	}
}

func unversionedNote(plan trafficPlan, unrouted []string) string {
	for _, label := range unrouted {
		if label == UnversionedLabel && plan.current.versioned {
			return " (engine.workerVersioning is off: either route the deployment to unversioned workers or set engine.workerVersioning: true)"
		}
	}
	return ""
}

// remediation names set-current-version or set-ramping-version commands
// that move traffic onto live workers. Each line is one alternative.
func remediation(plan trafficPlan, groups []pollerGroup) []string {
	var commands []string
	add := func(command string) {
		for _, existing := range commands {
			if existing == command {
				return
			}
		}
		commands = append(commands, command)
	}
	promoteLive := func() {
		for _, group := range groups {
			add(group.promoteCommand())
		}
	}
	if ramp := plan.ramp; ramp != nil && ramp.share > 0 && !ramp.live {
		if plan.current.live {
			add(ramp.rampDeleteCommand())
		} else {
			promoteLive()
		}
	}
	if plan.current.share > 0 && !plan.current.live {
		if plan.rampServed() {
			add(plan.ramp.promoteCommand())
		} else {
			promoteLive()
		}
	}
	for _, group := range groups {
		if plan.shareOf(group.version) > 0 {
			continue
		}
		if group.version != plan.current.version {
			add(group.promoteCommand())
		} else if plan.rampServed() {
			// Current at 0% under a full ramp: finish the rollout.
			add(plan.ramp.promoteCommand())
		}
	}
	return commands
}
