package v1alpha1

import "slices"

// These runtime wire types use time.Time, whose immutable values can be copied
// by assignment. Keep their copy implementations outside generated output:
// controller-gen otherwise assumes time.Time supplies DeepCopyInto, as the
// Kubernetes metav1.Time wrapper does. It still generates DeepCopy wrappers.

// DeepCopyInto copies the repair and its optional timestamps into out.
func (in *GaggleHealthRepair) DeepCopyInto(out *GaggleHealthRepair) {
	*out = *in
	if in.AttemptedAt != nil {
		value := *in.AttemptedAt
		out.AttemptedAt = &value
	}
	if in.CompletedAt != nil {
		value := *in.CompletedAt
		out.CompletedAt = &value
	}
}

// DeepCopyInto copies the finding and its mutable evidence into out.
func (in *GaggleHealthFinding) DeepCopyInto(out *GaggleHealthFinding) {
	*out = *in
	out.Evidence = slices.Clone(in.Evidence)
	in.Repair.DeepCopyInto(&out.Repair)
	if in.ResolvedAt != nil {
		value := *in.ResolvedAt
		out.ResolvedAt = &value
	}
	out.ResolutionEvidence = slices.Clone(in.ResolutionEvidence)
}

// DeepCopyInto copies the event and its optional finding into out.
func (in *GaggleHealthEvent) DeepCopyInto(out *GaggleHealthEvent) {
	*out = *in
	if in.Finding != nil {
		out.Finding = new(GaggleHealthFinding)
		in.Finding.DeepCopyInto(out.Finding)
	}
}

// DeepCopyInto copies the snapshot and its active and historical findings.
func (in *GaggleHealthSnapshot) DeepCopyInto(out *GaggleHealthSnapshot) {
	*out = *in
	if in.Active != nil {
		out.Active = make([]GaggleHealthFinding, len(in.Active))
		for i := range in.Active {
			in.Active[i].DeepCopyInto(&out.Active[i])
		}
	}
	if in.History != nil {
		out.History = make([]GaggleHealthFinding, len(in.History))
		for i := range in.History {
			in.History[i].DeepCopyInto(&out.History[i])
		}
	}
}

// DeepCopyInto copies controller status and its optional evaluation timestamp.
func (in *GaggleHealthControllerStatus) DeepCopyInto(out *GaggleHealthControllerStatus) {
	*out = *in
	if in.LastSuccessfulEvaluation != nil {
		value := *in.LastSuccessfulEvaluation
		out.LastSuccessfulEvaluation = &value
	}
}
