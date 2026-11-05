package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/operator"
)

func TestOptionsFromEnvironment(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want operator.Options
	}{
		{name: "defaults", want: operator.DefaultOptions()},
		{
			name: "overrides",
			env: map[string]string{
				"GOOBERS_OPERATOR_METRICS_ADDR":    "127.0.0.1:9080",
				"GOOBERS_OPERATOR_HEALTH_ADDR":     "127.0.0.1:9081",
				"GOOBERS_OPERATOR_LEADER_ELECTION": "true",
				"GOOBERS_OPERATOR_WORKER_IMAGE":    "registry.example/worker:v2",
			},
			want: operator.Options{MetricsAddr: "127.0.0.1:9080", HealthAddr: "127.0.0.1:9081", LeaderElection: true, WorkerImage: "registry.example/worker:v2"},
		},
		{
			name: "disabled endpoints and election",
			env: map[string]string{
				"GOOBERS_OPERATOR_METRICS_ADDR":    "0",
				"GOOBERS_OPERATOR_HEALTH_ADDR":     "0",
				"GOOBERS_OPERATOR_LEADER_ELECTION": "false",
			},
			want: operator.Options{MetricsAddr: "0", HealthAddr: "0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := optionsFromEnvironment(func(name string) string { return tt.env[name] })
			if err != nil || got != tt.want {
				t.Fatalf("options = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestOptionsFromEnvironmentRejectsInvalidBoolean(t *testing.T) {
	_, err := optionsFromEnvironment(func(name string) string {
		if name == "GOOBERS_OPERATOR_LEADER_ELECTION" {
			return "enabled"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "GOOBERS_OPERATOR_LEADER_ELECTION must be a boolean") {
		t.Fatalf("error = %v, want invalid leader-election setting", err)
	}
}
