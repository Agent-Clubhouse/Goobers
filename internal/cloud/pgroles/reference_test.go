package pgroles

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const referenceRoot = "../../.."

func documents(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(referenceRoot, path))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, chunk := range bytes.Split(data, []byte("\n---\n")) {
		var document map[string]any
		if err := yaml.Unmarshal(chunk, &document); err != nil {
			t.Fatal(err)
		}
		if document != nil {
			out = append(out, document)
		}
	}
	return out
}

func field(document map[string]any, path ...string) any {
	var value any = document
	for _, key := range path {
		value = value.(map[string]any)[key]
	}
	return value
}

// TestCNPGReferenceMatchesContract keeps the CNPG managed roles and Database
// owners equal to the roles and databases bootstrap provisions.
func TestCNPGReferenceMatchesContract(t *testing.T) {
	cluster := documents(t, "deploy/reference/goobers-cloud/cnpg/cluster.yaml")[0]
	if image := field(cluster, "spec", "imageName").(string); !strings.Contains(image, "@sha256:") {
		t.Fatalf("CNPG image %q is not pinned by digest", image)
	}
	for _, entry := range field(cluster, "spec", "topologySpreadConstraints").([]any) {
		spread := entry.(map[string]any)
		if spread["topologyKey"] == "topology.kubernetes.io/zone" && spread["whenUnsatisfiable"] != "DoNotSchedule" {
			t.Fatalf("CNPG zone spread is %v, want DoNotSchedule", spread["whenUnsatisfiable"])
		}
	}
	var roles []string
	for _, entry := range field(cluster, "spec", "managed", "roles").([]any) {
		role := entry.(map[string]any)
		if role["superuser"] == true || role["createrole"] == true || role["inRoles"] != nil {
			t.Fatalf("role %v has privileges beyond the contract", role["name"])
		}
		roles = append(roles, role["name"].(string))
	}
	if !slices.Equal(roles, Roles()) {
		t.Fatalf("CNPG managed roles = %v, want %v", roles, Roles())
	}
	want, err := Databases("example")
	if err != nil {
		t.Fatal(err)
	}
	var got []Database
	for _, database := range documents(t, "deploy/reference/goobers-cloud/cnpg/databases.yaml") {
		got = append(got, Database{Name: field(database, "spec", "name").(string), Owner: field(database, "spec", "owner").(string)})
	}
	if len(got) != len(want) {
		t.Fatalf("CNPG databases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Owner != want[i].Owner {
			t.Fatalf("CNPG database %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestAzureReferenceMatchesContract checks the Bicep module creates the same
// databases and is zone-redundant, private, and Entra-enabled.
func TestAzureReferenceMatchesContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(referenceRoot, "infra/bicep/modules/postgres.bicep"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, required := range []string{
		"var instanceDatabase = 'goobers_${replace(toLower(instanceName), '-', '_')}'",
		"'temporal'", "'temporal_visibility'",
		"mode: 'ZoneRedundant'", "publicNetworkAccess: 'Disabled'",
		"activeDirectoryAuth: 'Enabled'", "delegatedSubnetResourceId: delegatedSubnetId",
		"name: 'pgbouncer.enabled'",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("postgres.bicep lacks %q", required)
		}
	}
	for _, forbidden := range []string{"firewallRules", "'Burstable'", "publicNetworkAccess: 'Enabled'"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("postgres.bicep still contains %q", forbidden)
		}
	}
}
