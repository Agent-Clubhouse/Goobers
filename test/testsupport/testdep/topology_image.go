//go:build topology_image

package testdep

// The image-specific reference proof is a dedicated opt-in suite. Keep Docker
// out of the ordinary integration dependency inventory and provisioning gate.
func init() {
	declared["docker"] = Dependency{Name: "docker", InstallHint: "install Docker and prepare a local Goobers release base image"}
}
