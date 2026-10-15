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

func (p trafficPlan) currentServing() bool {
	return p.current.live && p.current.share > 0
}

// currentDescription names the current version, and the ramp beside it.
func (p trafficPlan) currentDescription() string {
	text := p.current.label()
	if p.ramp != nil {
		text += fmt.Sprintf(" (ramping %s at %s)", p.ramp.label(), percent(p.ramp.share))
	}
	return text
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

// liveGroups is the per-version verdict for each live poller group, by the
// same Check the worker logs with.
type liveGroups struct {
	unrouted, draining, unverified []pollerGroup
	all                            []pollerGroup
}

func (l liveGroups) labels(groups []pollerGroup) string {
	labels := make([]string, 0, len(groups))
	for _, group := range groups {
		labels = append(labels, group.label())
	}
	return strings.Join(labels, ", ")
}

// evaluate checks both directions of the routing: every version that
// receives a share of new workflow tasks needs a live worker, and every live
// worker build should receive a share unless it is draining. No live worker
// for any share stalls the engine; anything less is degraded.
func evaluate(taskQueue string, routing Routing, describeErr error, pollers []*taskqueuepb.PollerInfo, now time.Time) Assessment {
	groups := livePollerGroups(pollers, now)
	if len(groups) == 0 {
		return Unknown(taskQueue, fmt.Sprintf("no goobers worker has polled task queue %q for workflow tasks in the last %s, "+
			"so there is no live build to check the routing against", taskQueue, PollerMaxAge), now)
	}
	if describeErr != nil && !deploymentAbsent(describeErr) {
		return Unknown(taskQueue, fmt.Sprintf("cannot read worker deployment %q routing: %v", DeploymentName, describeErr), now)
	}
	a := Assessment{Deployment: DeploymentName, TaskQueue: taskQueue, CheckedAt: now.UTC()}
	plan := planTraffic(routing.WorkerDeploymentRoutingConfig, groups)
	if describeErr == nil {
		a.CurrentVersion = plan.current.label()
		if plan.ramp != nil {
			a.RampingVersion, a.RampingPercentage = plan.ramp.label(), plan.ramp.share
		}
	}
	live := liveGroups{all: groups}
	for _, group := range groups {
		finding := Check(group.versioned, group.build, routing, describeErr)
		worker := WorkerRouting{Version: group.label(), Pollers: group.count, Routing: WorkerRouted, TrafficPercent: plan.shareOf(group.version)}
		switch {
		case finding.Unverified:
			worker.Routing, worker.TrafficPercent = WorkerUnknown, 0
			live.unverified = append(live.unverified, group)
		case finding.Draining:
			worker.Routing = WorkerDraining
			live.draining = append(live.draining, group)
		case !finding.Routed():
			worker.Routing = WorkerUnrouted
			live.unrouted = append(live.unrouted, group)
		}
		a.Workers = append(a.Workers, worker)
	}
	if len(live.unverified) > 0 {
		// No deployment, yet a versioned worker polls: the deployment should
		// exist, so its routing is unknown rather than absent.
		a.State, a.Detail = StateUnknown, fmt.Sprintf("live workers serve %s, but worker deployment %q cannot be described: %v",
			live.labels(live.unverified), DeploymentName, describeErr)
		return a
	}
	a.Remediation = remediation(plan, live)
	a.State, a.Detail = verdict(a, plan, describeErr != nil, live)
	return a
}

func verdict(a Assessment, plan trafficPlan, absent bool, live liveGroups) (State, string) {
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
	if len(live.unrouted) > 0 {
		problems = append(problems, fmt.Sprintf("live workers serving %s receive no new workflow tasks%s",
			live.labels(live.unrouted), unversionedNote(plan, live.unrouted)))
	}
	routes := plan.describe()
	if absent {
		routes = "current " + UnversionedLabel + " (100%; no versioned worker has registered the deployment)"
	}
	if len(live.draining) > 0 {
		routes += fmt.Sprintf("; %s draining", live.labels(live.draining))
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
			a.Deployment, a.TaskQueue, live.labels(live.all), routes)
	}
}

func unversionedNote(plan trafficPlan, unrouted []pollerGroup) string {
	for _, group := range unrouted {
		if !group.versioned && plan.current.versioned {
			return " (unversioned workers: if they are the previous fleet shutting down, this clears when they exit; " +
				"otherwise they run with engine.workerVersioning off, so either set engine.workerVersioning: true for them " +
				"or route the deployment to unversioned workers)"
		}
	}
	return ""
}

// remediation names set-current-version or set-ramping-version commands
// that move traffic onto live workers. Each line is one alternative. It
// never suggests replacing a live, serving current version with a draining
// build or with unversioned workers, either of which would be a rollback.
func remediation(plan trafficPlan, live liveGroups) []string {
	var commands []string
	add := func(command string) {
		for _, existing := range commands {
			if existing == command {
				return
			}
		}
		commands = append(commands, command)
	}
	// With no share served, any live build is a way out.
	promoteLive := func() {
		for _, group := range live.all {
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
	for _, group := range live.unrouted {
		switch {
		case group.version == plan.current.version:
			// Current at 0% under a full ramp: finish the rollout.
			if plan.rampServed() {
				add(plan.ramp.promoteCommand())
			}
		case !group.versioned && plan.currentServing():
			// Old or misconfigured unversioned workers beside a serving
			// versioned fleet: the note says which way to go.
		default:
			// A build that was never current or ramping: promote it.
			add(group.promoteCommand())
		}
	}
	return commands
}
