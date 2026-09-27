package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestBuildSchedulerSetupStampsDisabledReasonOnWorkflowEntries(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, root string)
		want string
	}{
		{
			name: "workflow disabled",
			edit: func(t *testing.T, root string) {
				replaceInFile(t,
					filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml"),
					"spec:\n  gaggle: example\n",
					"spec:\n  gaggle: example\n  enabled: false\n",
				)
			},
			want: `workflow "default-implement" is disabled (spec.enabled=false)`,
		},
		{
			name: "gaggle disabled",
			edit: func(t *testing.T, root string) {
				replaceInFile(t,
					filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml"),
					"spec:\n",
					"spec:\n  enabled: false\n",
				)
			},
			want: `gaggle "example" is disabled (spec.enabled=false)`,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			root := initDeterministicDemo(t)
			test.edit(t, root)

			var wg sync.WaitGroup
			setup, err := buildSchedulerSetup(context.Background(), instance.NewLayout(root), &wg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = setup.Shutdown(context.Background()) }()

			for _, entry := range setup.Entries {
				if entry.Gaggle == "example" && entry.Workflow == "default-implement" {
					if entry.DisabledReason != test.want {
						t.Fatalf("DisabledReason = %q, want %q", entry.DisabledReason, test.want)
					}
					return
				}
			}
			t.Fatal("default-implement entry not found")
		})
	}
}
