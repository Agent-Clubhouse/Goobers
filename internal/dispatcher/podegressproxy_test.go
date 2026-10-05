package dispatcher

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnercap"
)

func proxyConfig() Config {
	cfg := testConfig()
	cfg.PodEgressProxy = &instance.PodEgressProxyConfig{
		HTTPSProxy: "http://goobers-egress-proxy.goobers-system:3128",
		HTTPProxy:  "http://goobers-egress-proxy.goobers-system:3128",
		NoProxy:    ".goobers-system,.svc",
	}
	return cfg
}

func allowlistRunner(extra ...string) RunnerSpec {
	r := linuxRunner()
	r.Restrictions = append(r.Restrictions, extra...)
	return r
}

func TestPodEgressProxyStampedOnAllowlistClassOnly(t *testing.T) {
	pod, err := RenderPod(proxyConfig(), testAttempt(), allowlistRunner())
	if err != nil {
		t.Fatal(err)
	}
	env := podEnv(pod)
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if env[name] != "http://goobers-egress-proxy.goobers-system:3128" {
			t.Errorf("%s = %q", name, env[name])
		}
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		if env[name] != ".goobers-system,.svc" {
			t.Errorf("%s = %q", name, env[name])
		}
	}

	open := linuxRunner()
	open.Restrictions = []string{"tmp:ephemeral"}
	pod, err = RenderPod(proxyConfig(), testAttempt(), open)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range podEgressProxyEnvNames {
		if _, ok := podEnv(pod)[name]; ok {
			t.Errorf("%s stamped on a class without network:allowlist", name)
		}
	}

	pod, err = RenderPod(testConfig(), testAttempt(), allowlistRunner())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range podEgressProxyEnvNames {
		if _, ok := podEnv(pod)[name]; ok {
			t.Errorf("%s stamped with no podEgressProxy configured", name)
		}
	}
}

func TestPodEgressProxyDoesNotOverrideStageEnv(t *testing.T) {
	attempt := testAttempt()
	attempt.Env = map[string]string{"HTTPS_PROXY": "http://stage-own:1", "NO_PROXY": "stage.local"}
	pod, err := RenderPod(proxyConfig(), attempt, allowlistRunner())
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, e := range pod.Spec.Containers[0].Env {
		count[e.Name]++
	}
	env := podEnv(pod)
	if env["HTTPS_PROXY"] != "http://stage-own:1" || env["NO_PROXY"] != "stage.local" || count["HTTPS_PROXY"] != 1 || count["NO_PROXY"] != 1 {
		t.Fatalf("stage env overridden or duplicated: %v %v", env, count)
	}
	if env["HTTP_PROXY"] == "" {
		t.Fatal("untouched proxy variables should still be stamped")
	}
}

func TestPodEgressProxySurvivesEnvDefaultDeny(t *testing.T) {
	pod, err := RenderPod(proxyConfig(), testAttempt(), allowlistRunner(string(runnercap.RestrictionEnvDefaultDeny)))
	if err != nil {
		t.Fatal(err)
	}
	var allow []string
	if err := json.Unmarshal([]byte(podEnv(pod)[EnvStageEnvAllow]), &allow); err != nil {
		t.Fatal(err)
	}
	for _, name := range podEgressProxyEnvNames {
		if !slices.Contains(allow, name) {
			t.Errorf("%s missing from the env:default-deny allowlist %v", name, allow)
		}
	}
}
