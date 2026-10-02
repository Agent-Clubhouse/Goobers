package providers

import (
	"slices"
	"testing"
)

func TestNormalizeLabelSpecs(t *testing.T) {
	got := normalizeLabelSpecs([]WorkItemLabel{
		{Name: "  Needs Design  ", Color: " #AABBCC ", Description: "keep"},
		{Name: "ready", Color: "##123456"},
	})
	want := []WorkItemLabel{
		{Name: "Needs Design", Color: "AABBCC", Description: "keep"},
		{Name: "ready", Color: "#123456"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("normalizeLabelSpecs = %#v, want %#v", got, want)
	}
}

func TestPlanLabelEnsure(t *testing.T) {
	steps := planLabelEnsure(
		[]string{"Existing"},
		[]WorkItemLabel{
			{Name: " existing ", Color: " #111111 "},
			{Name: "New", Color: "#222222"},
			{Name: "new", Color: "333333"},
			{Name: "Later", Color: "444444"},
		},
		lowerLabelName,
	)
	if got, want := len(steps), 4; got != want {
		t.Fatalf("len(planLabelEnsure) = %d, want %d", got, want)
	}
	gotNames := make([]string, 0, len(steps))
	gotCreate := make([]bool, 0, len(steps))
	for _, step := range steps {
		gotNames = append(gotNames, step.Label.Name)
		gotCreate = append(gotCreate, step.Create)
	}
	if want := []string{"existing", "New", "new", "Later"}; !slices.Equal(gotNames, want) {
		t.Fatalf("names = %v, want %v", gotNames, want)
	}
	if want := []bool{false, true, false, true}; !slices.Equal(gotCreate, want) {
		t.Fatalf("create steps = %v, want %v", gotCreate, want)
	}
}

func TestPlanLabelMutation(t *testing.T) {
	tests := []struct {
		name    string
		current []string
		add     []string
		remove  []string
		fold    labelNameFold
		want    labelMutationPlan
	}{
		{
			name:    "exact ordering and duplicate suppression",
			current: []string{"first", "drop", "keep", "first"},
			add:     []string{"new", "keep", "new"},
			remove:  []string{"drop", "drop"},
			fold:    exactLabelName,
			want: labelMutationPlan{
				Add:    []string{"new"},
				Remove: []string{"drop"},
				Result: []string{"first", "keep", "new"},
			},
		},
		{
			name:    "ADO matches without changing canonical spelling",
			current: []string{"Route/Backend", "GOOBERS:CLAIMED"},
			add:     []string{"route/backend", "goobers/status:in-progress", "GOOBERS/STATUS:IN-PROGRESS"},
			remove:  []string{"goobers:claimed"},
			fold:    equalFoldLabelName,
			want: labelMutationPlan{
				Add:    []string{"goobers/status:in-progress"},
				Remove: []string{"goobers:claimed"},
				Result: []string{"Route/Backend", "goobers/status:in-progress"},
			},
		},
		{
			name:    "unknown current plans remove for benign 404",
			current: nil,
			add:     []string{"add", "add"},
			remove:  []string{"possibly-absent", "possibly-absent"},
			fold:    exactLabelName,
			want: labelMutationPlan{
				Add:    []string{"add"},
				Remove: []string{"possibly-absent"},
				Result: []string{"add"},
			},
		},
		{
			name:    "add wins over remove",
			current: []string{"replace"},
			add:     []string{"replace"},
			remove:  []string{"replace"},
			fold:    exactLabelName,
			want: labelMutationPlan{
				Add:    []string{"replace"},
				Remove: []string{"replace"},
				Result: []string{"replace"},
			},
		},
		{
			name:    "trims and drops blank mutation inputs",
			current: []string{" first ", " ", "drop", "first"},
			add:     []string{" new ", "", " first "},
			remove:  []string{" drop ", "\t"},
			fold:    exactLabelName,
			want: labelMutationPlan{
				Add:    []string{"new"},
				Remove: []string{"drop"},
				Result: []string{"first", "new"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planLabelMutation(tt.current, tt.add, tt.remove, tt.fold)
			if !slices.Equal(got.Add, tt.want.Add) ||
				!slices.Equal(got.Remove, tt.want.Remove) ||
				!slices.Equal(got.Result, tt.want.Result) {
				t.Fatalf("planLabelMutation = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReplaceStatusLabel(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		status WorkItemStatus
		want   []string
	}{
		{
			name:   "replaces statuses in place with one trailing status",
			labels: []string{"first", "goobers/status:open", "keep", "goobers/status:claimed", "first"},
			status: WorkItemStatusInProgress,
			want:   []string{"first", "keep", "goobers/status:in-progress"},
		},
		{
			name:   "empty status removes status labels",
			labels: []string{"goobers/status:open", "keep"},
			want:   []string{"keep"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replaceStatusLabel(tt.labels, tt.status); !slices.Equal(got, tt.want) {
				t.Fatalf("replaceStatusLabel = %v, want %v", got, tt.want)
			}
		})
	}
}
