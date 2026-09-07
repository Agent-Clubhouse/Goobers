package main

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Validate the same lifecycle vocabulary onboarding provisions. In particular,
// readyLabel/resweepReadyLabel defaults and policy-action claims need not appear
// as literal selector inputs, but still depend on repository labels (#3184).
func appendTaskLifecycleLabelUses(demand *repoRealityDemand, task apiv1.Task, where, file, path string) {
	appendTaskParkLabelUses(demand, task, where, file, path)
	labels := &connectLabelSet{}
	connectTaskAppliedLabels(task, labels)
	for _, label := range labels.sorted() {
		alreadyReported := false
		for _, use := range demand.labelUses {
			if strings.EqualFold(use.label, label) && use.file == file && strings.HasPrefix(use.where, where) {
				alreadyReported = true
				break
			}
		}
		if !alreadyReported {
			demand.labelUses = append(demand.labelUses, labelUse{
				label: label, kind: labelUseApply, where: where + " lifecycle label",
				file: file, path: path,
			})
		}
	}
}

func appendTaskParkLabelUses(demand *repoRealityDemand, task apiv1.Task, where, file, path string) {
	for _, label := range splitLabelList(task.Inputs["parkLabels"]) {
		demand.labelUses = append(demand.labelUses, labelUse{
			label: label, kind: labelUseExclude, where: where + " inputs.parkLabels",
			file: file, path: path + "/parkLabels",
		})
	}
}
