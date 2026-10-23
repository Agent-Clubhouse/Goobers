package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func TestTemporalLadderAdvancesOneMinorToWorkerDeploymentGA(t *testing.T) {
	t.Parallel()
	var got []string
	for _, step := range temporalLadder {
		got = append(got, step.server+"/"+step.chart)
	}
	want := []string{
		"1.27.2/temporal.tgz",
		"1.27.4/temporal.tgz",
		"1.28.4/temporal-0.67.0.tgz",
		"1.29.7/temporal-0.73.2.tgz",
		"1.30.8/temporal-0.74.0.tgz",
		"1.31.4/temporal-1.6.0.tgz",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Temporal ladder = %v, want %v", got, want)
	}
}

func TestTemporalLadderValuesPinVersioningAndSchemaFirst(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"../../../../../deploy/reference/temporal/values.yaml",
		"../../../../../infra/temporal/values.yaml",
		"../../temporal-values-legacy.yaml",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		text := string(data)
		for _, want := range []string{
			"frontend.workerVersioningWorkflowAPIs:",
			"system.enableDeploymentVersions:",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s is missing %q", path, want)
			}
		}

		if strings.Contains(path, "values-legacy") {
			if !strings.Contains(text, "update:\n    enabled: true") {
				t.Errorf("%s does not enable the legacy schema update job", path)
			}
		} else if !strings.Contains(text, "manageSchema: true") ||
			!strings.Contains(text, "useHelmHooks: true") ||
			strings.Count(text, "connectProtocol: tcp") != 2 {
			t.Errorf("%s does not carry the complete chart 1.x schema and SQL connection contract", path)
		}
	}

}

func TestShardRangesAdvancedRequiresEveryShardToMove(t *testing.T) {
	t.Parallel()
	before := map[int64]int64{0: 17, 1: 22, 2: 9, 3: 41}
	for _, test := range []struct {
		name    string
		current map[int64]int64
		want    bool
	}{
		{"all advanced", map[int64]int64{0: 18, 1: 23, 2: 10, 3: 42}, true},
		{"pre-existing rows", map[int64]int64{0: 17, 1: 22, 2: 9, 3: 41}, false},
		{"one stale shard", map[int64]int64{0: 18, 1: 22, 2: 10, 3: 42}, false},
		{"missing shard", map[int64]int64{0: 18, 1: 23, 2: 10}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shardRangesAdvanced(before, test.current); got != test.want {
				t.Fatalf("shardRangesAdvanced() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestShardSettlementRejectsOneUnadvancedShard(t *testing.T) {
	before := map[int64]int64{1: 17, 2: 17, 3: 17, 4: 20}
	post := map[int64]int64{1: 18, 2: 18, 3: 18, 4: 20}
	query := func(context.Context) (map[int64]int64, string, error) {
		return post, "1|18\n2|18\n3|18\n4|20", nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := waitForShardSettlementWithQuery(ctx, t.TempDir(), "test", before, query, 0, time.Millisecond); err == nil {
		t.Fatal("settlement accepted a post-rollout map whose fourth shard did not advance")
	}
}

func TestShardSettlementDwellsAndRechecksAdvancedMap(t *testing.T) {
	before := map[int64]int64{1: 17, 2: 17, 3: 17, 4: 20}
	post := map[int64]int64{1: 18, 2: 18, 3: 18, 4: 21}
	calls := 0
	query := func(context.Context) (map[int64]int64, string, error) {
		calls++
		return post, "1|18\n2|18\n3|18\n4|21", nil
	}
	if err := waitForShardSettlementWithQuery(t.Context(), t.TempDir(), "test", before, query, 0, 0); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("query calls = %d, want at least two so the post-dwell map is rechecked", calls)
	}
}

// The kind database follows the production role contract: every credential
// Secret the cluster references is created by the runner with a distinct
// random password, and the Temporal ladder connects as temporal_app.
func TestKindDatabaseFollowsRoleContract(t *testing.T) {
	manifest, err := databaseCredentialsManifest()
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := os.ReadFile("../../cnpg-cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	passwords := map[string]bool{}
	for _, credential := range databaseCredentials {
		if !strings.Contains(manifest, "name: "+credential.secret+"\n") || !strings.Contains(manifest, "username: "+credential.user+"\n") {
			t.Fatalf("manifest missing %s/%s", credential.secret, credential.user)
		}
		if !strings.Contains(string(cluster), "name: "+credential.secret+"\n") {
			t.Fatalf("cnpg-cluster.yaml does not reference Secret %s", credential.secret)
		}
	}
	for _, line := range strings.Split(manifest, "\n") {
		if password, ok := strings.CutPrefix(strings.TrimSpace(line), "password: "); ok {
			if len(password) != 48 || passwords[password] {
				t.Fatalf("passwords must be distinct 48-hex values, got %q", password)
			}
			passwords[password] = true
		}
	}
	if len(passwords) != len(databaseCredentials) {
		t.Fatalf("got %d passwords, want %d", len(passwords), len(databaseCredentials))
	}
	// CNPG runs postInitSQL on the postgres database BEFORE it creates the
	// bootstrap database, so a statement naming that database must be in
	// postInitApplicationSQL or initdb fails on every retry and no primary
	// ever starts. temporal_visibility is created inside postInitSQL itself.
	var spec struct {
		Spec struct {
			Bootstrap struct {
				Initdb struct {
					Database               string   `json:"database"`
					PostInitSQL            []string `json:"postInitSQL"`
					PostInitApplicationSQL []string `json:"postInitApplicationSQL"`
				} `json:"initdb"`
			} `json:"bootstrap"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(cluster, &spec); err != nil {
		t.Fatal(err)
	}
	initdb := spec.Spec.Bootstrap.Initdb
	contains := func(list []string, want string) bool { return slices.Contains(list, want) }
	if !contains(initdb.PostInitApplicationSQL, "REVOKE ALL ON DATABASE temporal FROM PUBLIC") {
		t.Fatalf("revoke on the bootstrap database must be in postInitApplicationSQL, got %v", initdb.PostInitApplicationSQL)
	}
	if contains(initdb.PostInitSQL, "REVOKE ALL ON DATABASE temporal FROM PUBLIC") {
		t.Fatal("postInitSQL runs before the bootstrap database exists; the temporal revoke would fail every initdb attempt")
	}
	create := slices.Index(initdb.PostInitSQL, "CREATE DATABASE temporal_visibility OWNER temporal_app")
	revoke := slices.Index(initdb.PostInitSQL, "REVOKE ALL ON DATABASE temporal_visibility FROM PUBLIC")
	if create < 0 || revoke < create {
		t.Fatalf("temporal_visibility must be created before it is revoked in postInitSQL, got %v", initdb.PostInitSQL)
	}
	for _, want := range []string{"log_min_error_statement: panic", "owner: temporal_app", "enableSuperuserAccess: true", "goobers_owner", "goobers_api", "goobers_controller"} {
		if !strings.Contains(string(cluster), want) {
			t.Fatalf("cnpg-cluster.yaml missing %q", want)
		}
	}
	if strings.Contains(string(cluster), "ON DATABASE postgres") {
		t.Fatal("do not revoke from the postgres database: CNPG pg_rewind connects to it as streaming_replica")
	}
	for _, path := range []string{"../../temporal-values.yaml", "../../temporal-values-legacy.yaml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "user: temporal\n") || strings.Count(string(data), "user: temporal_app\n") != 2 {
			t.Fatalf("%s must connect both datastores as temporal_app", path)
		}
	}
}
