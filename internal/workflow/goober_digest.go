package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type effectiveGoober struct {
	Name           string                     `json:"name"`
	Instructions   string                     `json:"instructions"`
	Skills         []string                   `json:"skills,omitempty"`
	SkillPackages  []effectiveSkillPackage    `json:"skillPackages,omitempty"`
	Model          string                     `json:"model,omitempty"`
	Harness        string                     `json:"harness"`
	HarnessOptions map[string]json.RawMessage `json:"harnessOptions,omitempty"`
	MCPServers     []apiv1.MCPServer          `json:"mcpServers,omitempty"`
	Tools          []string                   `json:"tools,omitempty"`
}

type effectiveSkillPackage struct {
	Name  string      `json:"name"`
	Files []SkillFile `json:"files,omitempty"`
}

// SkillFile is one canonically named regular file in a declared skill package.
type SkillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ComputeGooberDigest returns the stable content identity of the resolved
// goobers that participate in def.
func ComputeGooberDigest(
	def Definition,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
	skillPackages map[string][]SkillFile,
) (string, error) {
	names := participatingGoobers(def)
	effective := make([]effectiveGoober, 0, len(names))
	for _, name := range names {
		spec, ok := goobers[name]
		if !ok {
			return "", fmt.Errorf("participating goober %q is not defined", name)
		}
		content, ok := instructions[name]
		if !ok {
			return "", fmt.Errorf("participating goober %q has no resolved instructions", name)
		}
		harness := spec.Harness
		if harness == "" {
			harness = apiv1.HarnessCopilot
		}
		options := make(map[string]json.RawMessage, len(spec.HarnessOptions))
		for key, value := range spec.HarnessOptions {
			if !json.Valid(value.Raw) {
				return "", fmt.Errorf("participating goober %q harness option %q is not valid JSON", name, key)
			}
			options[key] = append(json.RawMessage(nil), value.Raw...)
		}
		if len(options) == 0 {
			options = nil
		}
		effective = append(effective, effectiveGoober{
			Name:           name,
			Instructions:   content,
			Skills:         canonicalSet(spec.Skills),
			SkillPackages:  resolvedSkillPackages(spec.Skills, spec.Gaggle == "", skillPackages),
			Model:          spec.Model,
			Harness:        string(harness),
			HarnessOptions: options,
			MCPServers:     canonicalMCPServers(spec.MCPServers),
			Tools:          canonicalSet(spec.Tools),
		})
	}
	return canonicalDigest(effective)
}

// SharedSkillPackageKey keeps an instance persona's skill separate from a
// same-named gaggle override in the captured workflow skill package map.
// Skill names cannot contain slashes, so this namespace cannot collide with
// an ordinary skill name. The namespace is not part of the content digest.
func SharedSkillPackageKey(name string) string { return "shared/" + name }

func resolvedSkillPackages(skills []string, shared bool, packages map[string][]SkillFile) []effectiveSkillPackage {
	var resolved []effectiveSkillPackage
	for _, name := range canonicalSet(skills) {
		files, ok := packages[name]
		if shared {
			// Preserve compatibility with callers supplying a single, already
			// resolved package set; captured mixed-scope sets use explicit keys.
			if scoped, exists := packages[SharedSkillPackageKey(name)]; exists {
				files, ok = scoped, true
			}
		}
		if ok {
			canonical := append([]SkillFile(nil), files...)
			sort.Slice(canonical, func(i, j int) bool { return canonical[i].Path < canonical[j].Path })
			resolved = append(resolved, effectiveSkillPackage{Name: name, Files: canonical})
		}
	}
	return resolved
}

func canonicalMCPServers(servers []apiv1.MCPServer) []apiv1.MCPServer {
	if len(servers) == 0 {
		return nil
	}
	out := make([]apiv1.MCPServer, len(servers))
	for i := range servers {
		out[i] = servers[i]
		out[i].Args = append([]string(nil), servers[i].Args...)
		out[i].CredentialRefs = append([]apiv1.MCPCredentialRef(nil), servers[i].CredentialRefs...)
		sort.Slice(out[i].CredentialRefs, func(a, b int) bool {
			left, right := out[i].CredentialRefs[a], out[i].CredentialRefs[b]
			return left.Capability+"\x00"+string(left.Kind)+"\x00"+left.Ref+"\x00"+left.Env+"\x00"+left.Header+"\x00"+string(left.Scheme) <
				right.Capability+"\x00"+string(right.Kind)+"\x00"+right.Ref+"\x00"+right.Env+"\x00"+right.Header+"\x00"+string(right.Scheme)
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func participatingGoobers(def Definition) []string {
	names := map[string]struct{}{}
	for _, task := range def.Spec.Tasks {
		if task.Type == apiv1.TaskAgentic && task.Goober != "" {
			names[task.Goober] = struct{}{}
		}
	}
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorAgentic && gate.Agentic != nil && gate.Agentic.Goober != "" {
			names[gate.Agentic.Goober] = struct{}{}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func canonicalSet(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func canonicalDigest(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
