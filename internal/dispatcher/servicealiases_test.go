package dispatcher

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func aliasConfig() Config {
	cfg := testConfig()
	cfg.WriteAPIBase, cfg.BlobEndpoint = "https://daemon.system.svc:8080", "https://blob.system.svc:8080"
	cfg.NetworkNoneHostAliases = []corev1.HostAlias{{IP: "10.0.0.1", Hostnames: []string{"daemon.system.svc"}}, {IP: "10.0.0.2", Hostnames: []string{"blob.system.svc"}}}
	return cfg
}

func TestNetworkNoneServiceAliasesBothRenderPaths(t *testing.T) {
	cfg, runner := aliasConfig(), linuxRunner()
	runner.Restrictions = []string{"network:none"}
	for _, template := range []bool{false, true} {
		var pod *corev1.Pod
		var err error
		if template {
			dep := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "stage", Image: runner.Host}}, HostAliases: []corev1.HostAlias{{IP: "192.0.2.1", Hostnames: []string{"DAEMON.SYSTEM.SVC", "operator.internal"}}}}}}}
			pod, err = RenderFromTemplate(cfg, testAttempt(), runner, dep)
			if dep.Spec.Template.Spec.HostAliases[0].Hostnames[0] != "DAEMON.SYSTEM.SVC" {
				t.Fatal("mutated template")
			}
		} else {
			pod, err = RenderPod(cfg, testAttempt(), runner)
		}
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]string{}
		for _, alias := range pod.Spec.HostAliases {
			for _, name := range alias.Hostnames {
				found[name] = alias.IP
			}
		}
		if found["daemon.system.svc"] != "10.0.0.1" || found["blob.system.svc"] != "10.0.0.2" || found["DAEMON.SYSTEM.SVC"] != "" {
			t.Fatalf("aliases=%v", found)
		}
		if template && found["operator.internal"] != "192.0.2.1" {
			t.Fatal("lost unrelated template alias")
		}
	}
	runner.Restrictions = nil
	pod, err := RenderPod(cfg, testAttempt(), runner)
	if err != nil || len(pod.Spec.HostAliases) != 0 {
		t.Fatalf("unrestricted pod changed: %+v %v", pod, err)
	}
	cfg.NetworkNoneHostAliases = nil
	runner.Restrictions = []string{"network:none"}
	if _, err = RenderPod(cfg, testAttempt(), runner); err == nil || !strings.Contains(err.Error(), "NETWORK_NONE_SERVICE") {
		t.Fatalf("missing aliases accepted: %v", err)
	}
}

func TestNetworkNoneAliasesResolveLiveServiceAndRefuseUnstableEndpoints(t *testing.T) {
	cfg := aliasConfig()
	cfg.BlobEndpoint = cfg.WriteAPIBase
	runner := linuxRunner()
	runner.Restrictions = []string{"network:none"}
	client := fake.NewClientset(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "daemon", Namespace: "system"}, Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.3"}})
	d := &Dispatcher{cfg: cfg, pods: NewKubernetesPodAPI(client)}
	for _, ip := range []string{"10.0.0.3", "10.0.0.4"} {
		service, _ := client.CoreV1().Services("system").Get(context.Background(), "daemon", metav1.GetOptions{})
		service.Spec.ClusterIP = ip
		if _, err := client.CoreV1().Services("system").Update(context.Background(), service, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		pod, err := d.renderHost(context.Background(), testAttempt(), runner)
		if err != nil || len(pod.Spec.HostAliases) != 1 || pod.Spec.HostAliases[0].IP != ip {
			t.Fatalf("pod=%+v err=%v", pod, err)
		}
	}
	for _, endpoint := range []string{"https://example.org.invalid", "https://10.0.0.3", "https://missing.system.svc"} {
		d.cfg.WriteAPIBase = endpoint
		if _, err := d.renderHost(context.Background(), testAttempt(), runner); err == nil || !strings.Contains(err.Error(), "NETWORK_NONE_SERVICE") {
			t.Fatalf("endpoint=%s err=%v", endpoint, err)
		}
	}
	d.cfg = cfg
	for _, typ := range []corev1.ServiceType{corev1.ServiceTypeExternalName, corev1.ServiceTypeClusterIP} {
		service, _ := client.CoreV1().Services("system").Get(context.Background(), "daemon", metav1.GetOptions{})
		service.Spec.Type = typ
		service.Spec.ClusterIP = corev1.ClusterIPNone
		if _, err := client.CoreV1().Services("system").Update(context.Background(), service, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.renderHost(context.Background(), testAttempt(), runner); err == nil {
			t.Fatal("unstable service accepted")
		}
	}
}

func TestHostAliasHTTPSPreservesNameWithoutDNS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "example.com:") {
			t.Errorf("Host=%s", r.Host)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	addr, _ := url.Parse(server.URL)
	host, port, _ := net.SplitHostPort(addr.Host)
	cfg := aliasConfig()
	cfg.WriteAPIBase = "https://example.com:" + port
	cfg.BlobEndpoint = cfg.WriteAPIBase
	cfg.NetworkNoneHostAliases = []corev1.HostAlias{{IP: host, Hostnames: []string{"example.com"}}}
	// Use the shared host-alias helper for both OS shapes. Windows network:none
	// remains undeclarable; this does not change its restriction eligibility.
	for _, os := range []string{"linux", "windows"} {
		spec := corev1.PodSpec{OS: &corev1.PodOS{Name: corev1.OSName(os)}}
		if err := stampServiceAliases(cfg, &spec, RunnerSpec{OS: os, Restrictions: []string{"network:none"}}); err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(server.Certificate())
		resolver := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) { return nil, fmt.Errorf("DNS disabled") }}
		dialer := &net.Dialer{Resolver: resolver}
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			name, p, _ := net.SplitHostPort(address)
			for _, alias := range spec.HostAliases {
				for _, h := range alias.Hostnames {
					if h == name {
						return dialer.DialContext(ctx, network, net.JoinHostPort(alias.IP, p))
					}
				}
			}
			return dialer.DialContext(ctx, network, address)
		}}
		client := &http.Client{Transport: transport}
		response, err := client.Get(cfg.WriteAPIBase)
		if err != nil {
			t.Fatalf("%s HTTPS failed: %v", os, err)
		}
		_ = response.Body.Close()
		transport.CloseIdleConnections()
		if response.StatusCode != 200 {
			t.Fatal(response.Status)
		}
	}
}

func TestNetworkNoneWindowsStillRefused(t *testing.T) {
	runner := windowsRunner()
	runner.Restrictions = []string{"network:none"}
	if _, err := RenderPod(aliasConfig(), testAttempt(), runner); err == nil {
		t.Fatal("Windows restriction was lifted")
	}
}

func TestHostAliasHelperLeavesOtherClassesUnchanged(t *testing.T) {
	spec := corev1.PodSpec{HostAliases: []corev1.HostAlias{{IP: "127.0.0.1", Hostnames: []string{"unchanged"}}}}
	want := spec.DeepCopy()
	if err := stampServiceAliases(aliasConfig(), &spec, linuxRunner()); err != nil || !reflect.DeepEqual(spec, *want) {
		t.Fatalf("spec=%+v err=%v", spec, err)
	}
}
