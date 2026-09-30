package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry"
	"gopkg.in/yaml.v3"
)

var mode atomic.Int32
var modeRequests [6]atomic.Int64
var requests, received, encoded, rejects atomic.Int64
var ids sync.Map
var duplicates atomic.Int64
var streamMu sync.Mutex
var streams = map[string]int{}
var bin, out string
var workers int
var recoveryAfter time.Duration
var pollInterval time.Duration
var azureConnectionEnv string
var collectionProfile string
var sampleInterval time.Duration
var windowsInsecureDemo bool
var settleTimeout time.Duration
var diskFullVolume string
var journalKeys sync.Map
var checkpointIDs []string
var client = &http.Client{Timeout: 3 * time.Second}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func write(path, body string) {
	must(os.MkdirAll(filepath.Dir(path), 0700))
	must(os.WriteFile(path, []byte(body), 0600))
}
func env() []string {
	var e []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GOOBERS_") && !strings.HasPrefix(v, "OTEL_") && !strings.HasPrefix(v, "APPLICATIONINSIGHTS_") && !strings.HasPrefix(v, "GITHUB_") && !strings.HasPrefix(v, "GH_") && !strings.HasPrefix(v, "AZURE_") && !strings.HasPrefix(v, "ADO_") && !strings.HasPrefix(v, "COPILOT_") {
			e = append(e, v)
		}
	}
	return e
}
func cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, bin, args...)
	c.Env = env()
	if runtime.GOOS == "windows" && windowsInsecureDemo {
		// The explicit fixture flag covers both scaffolding and execution.
		// Never inherit this opt-out from the caller's ambient environment.
		c.Env = append(c.Env, "GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE=1")
	}
	if azureConnectionEnv != "" {
		c.Env = append(c.Env, azureConnectionEnv+"="+os.Getenv(azureConnectionEnv))
	}
	return c
}

type Result struct {
	CatchupWaitMS                                       float64
	MetricSampleErrors                                  int
	ExternalReceiver                                    bool
	Name                                                string
	StartupMS, ShutdownMS                               float64
	Runs, Failures, HealthFailures                      int
	RunP50MS, RunP95MS, HealthP95MS                     float64
	MaxRSSKiB                                           int64
	MeanCPU                                             float64
	Requests, Records, Rejected, Duplicates             int64
	Streams                                             map[string]int
	NetworkModes                                        map[string]int64
	DiskFullConfirmed                                   bool
	DiskFullBytes                                       int64
	Replay                                              telemetry.AzureReplayStats
	DiskKiB                                             int64
	ExpectedRunEvents, MissingRunEvents                 int
	CPUSeconds, WallSeconds                             float64
	PersistedCheckpointRecords, MissingPersistedRecords int
}

func percentile(a []float64, p float64) float64 {
	if len(a) == 0 {
		return 0
	}
	sort.Float64s(a)
	return a[int(float64(len(a)-1)*p)]
}
func spool(root string) string { return filepath.Join(root, "telemetry-export", "azure-monitor") }

func prefill(root string, mb, tiny int, legacy bool) {
	dir := filepath.Join(spool(root), "journal")
	must(os.MkdirAll(dir, 0700))
	var total int
	for i := 0; (tiny > 0 && i < tiny) || (tiny == 0 && total < mb<<20); i++ {
		n, padding := 128, 3800
		if tiny > 0 {
			n, padding = 1, 64
		}
		var body strings.Builder
		h := map[string]any{"schema": "goobers.dev/telemetry/azure-replay/v1", "createdAt": time.Now().UTC().Add(-time.Hour)}
		if !legacy {
			h["records"] = n
		}
		b, _ := json.Marshal(h)
		body.Write(b)
		body.WriteByte('\n')
		for j := 0; j < n; j++ {
			b, _ := json.Marshal(map[string]any{"name": "Microsoft.ApplicationInsights.Message", "time": time.Now().UTC(), "iKey": "00000000-0000-0000-0000-000000000000", "data": map[string]any{"baseType": "MessageData", "baseData": map[string]any{"ver": 2, "message": strings.Repeat("x", padding), "properties": map[string]string{"goobers.telemetry.record_id": fmt.Sprintf("seed-%s-%d-%d", filepath.Base(root), i, j)}}}})
			body.Write(b)
			body.WriteByte('\n')
		}
		write(filepath.Join(dir, fmt.Sprintf("%020d-seed.ndjson", i)), body.String())
		total += body.Len()
	}
	fmt.Printf("prefill %s bytes=%d tiny=%d legacy=%v\n", filepath.Base(root), total, tiny, legacy)
}

func setup(name, url string, enabled bool) (string, string) {
	root := filepath.Join(out, name)
	args := []string{"init", "--demo", "--allow-ephemeral"}
	if runtime.GOOS == "windows" {
		if !windowsInsecureDemo {
			panic("Windows synthetic demo requires explicit -windows-insecure-demo; it runs without network isolation")
		}
		args = append(args, "--insecure")
	}
	args = append(args, root)
	b, err := cmd(context.Background(), args...).CombinedOutput()
	write(filepath.Join(out, name+"-init.log"), string(b))
	if err != nil {
		panic(string(b))
	}
	cfg, err := instance.LoadConfig(filepath.Join(root, "instance.yaml"))
	must(err)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	addr := l.Addr().String()
	must(l.Close())
	cfg.API.Listen = addr
	cfg.RunConditions.MaxParallelRuns = max(8, workers*2)
	cfg.Telemetry.Diagnostics = &instance.DiagnosticsConfig{HeartbeatInterval: "10s"}
	if enabled {
		cfg.Telemetry.CollectionProfile = instance.TelemetryCollectionProfile(collectionProfile)
		cfg.Telemetry.AzureMonitor = &instance.AzureMonitorConfig{ConnectionString: instance.TokenRef{File: filepath.Join(root, "connection.txt")}}
		if azureConnectionEnv != "" {
			cfg.Telemetry.AzureMonitor.ConnectionString = instance.TokenRef{Env: azureConnectionEnv}
		} else {
			write(filepath.Join(root, "connection.txt"), "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint="+url)
		}
	}
	must(instance.WriteConfig(filepath.Join(root, "instance.yaml"), cfg))
	// Keep the existing demo definition, adding a second gaggle and five copies per gaggle.
	raw, err := os.ReadFile(filepath.Join(root, "config/gaggles/demo/workflows/demo.yaml"))
	must(err)
	must(checkOfflineDemoFixture(cfg, raw))
	gag, err := os.ReadFile(filepath.Join(root, "config/gaggles/demo/gaggle.yaml"))
	must(err)
	manifest, err := os.ReadFile(filepath.Join(root, "config/manifest.yaml"))
	must(err)
	write(filepath.Join(root, "config/manifest.yaml"), strings.Replace(string(manifest), "- demo", "- demo\n    - synthetic", 1))
	write(filepath.Join(root, "config/gaggles/synthetic/gaggle.yaml"), strings.ReplaceAll(string(gag), "demo", "synthetic"))
	for _, g := range []string{"demo", "synthetic"} {
		for i := 0; i < 5; i++ {
			s := strings.Replace(string(raw), "name: demo", fmt.Sprintf("name: load%d", i), 1)
			s = strings.Replace(s, "gaggle: demo", "gaggle: "+g, 1)
			s = strings.Replace(s, "maxConcurrentRuns: 1", "maxConcurrentRuns: 8", 1)
			write(filepath.Join(root, "config/gaggles", g, "workflows", fmt.Sprintf("load%d.yaml", i)), s)
		}
	}
	return root, "http://" + addr
}

// Keep the load fixture independent of GitHub even when the host can reach it
// (notably the explicit Windows demo exception to network isolation). The
// project/backlog labels in the demo gaggle are fixture data, not connections.
func checkOfflineDemoFixture(cfg *instance.Config, raw []byte) error {
	if len(cfg.Repos) != 0 {
		return fmt.Errorf("synthetic demo configured %d repository connections", len(cfg.Repos))
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		return fmt.Errorf("parse synthetic demo workflow: %w", err)
	}
	if len(workflow.Spec.Tasks) != 4 {
		return fmt.Errorf("synthetic demo has %d stages, expected four offline stages", len(workflow.Spec.Tasks))
	}
	for _, task := range workflow.Spec.Tasks {
		if task.Run == nil || task.Run.Network != apiv1.NetworkNone ||
			task.Run.Workspace != apiv1.WorkspaceScratch || task.Run.Script != "" ||
			len(task.Run.Command) != 3 || task.Run.Command[0] != "goobers" ||
			task.Run.Command[1] != "__demo-provider" || task.Run.Command[2] != task.Name ||
			len(task.Capabilities) != 0 {
			return fmt.Errorf("synthetic demo stage %q is not an offline demo-provider command", task.Name)
		}
	}
	return nil
}

func main() {
	var duration time.Duration
	var selected string
	flag.StringVar(&bin, "bin", "bin/goobers", "")
	flag.StringVar(&out, "out", "", "")
	flag.DurationVar(&duration, "duration", 90*time.Second, "")
	flag.StringVar(&selected, "scenario", "all", "")
	flag.IntVar(&workers, "workers", 4, "")
	flag.DurationVar(&recoveryAfter, "recovery-after", 0, "override recovery time for prefill scenarios")
	flag.DurationVar(&pollInterval, "poll-interval", 0, "pause per worker between workflows; use 3m with 10 workers for representative polling")
	flag.DurationVar(&sampleInterval, "sample-interval", time.Second, "process and health sampling interval; use 10s for long soaks")
	flag.StringVar(&collectionProfile, "profile", "standard", "health, journal, standard or diagnostic collection profile")
	flag.BoolVar(&windowsInsecureDemo, "windows-insecure-demo", false, "explicitly allow the bundled credential-free demo without network isolation on Windows")
	flag.DurationVar(&settleTimeout, "settle-timeout", 2*time.Minute, "maximum journal reconciliation wait after workload ends, before shutdown")
	flag.StringVar(&azureConnectionEnv, "azure-connection-env", "", "explicit connection-string environment reference; only with -scenario azure (no volume prefills)")
	flag.StringVar(&diskFullVolume, "disk-full-volume", "", "dedicated empty Linux tmpfs <=64MiB, containing only .goobers-telemetry-load-volume; only with -scenario disk-full")
	registerStartupFlags()
	flag.Parse()
	must(startupOptions.validate(selected))
	if (selected == "disk-full") != (diskFullVolume != "") {
		panic("disk-full requires an explicit dedicated -disk-full-volume, and that flag is only allowed for disk-full")
	}
	if diskFullVolume != "" {
		must(validateDiskFullVolume(diskFullVolume))
	}
	if azureConnectionEnv != "" && (selected != "azure" || os.Getenv(azureConnectionEnv) == "") {
		panic("Azure validation requires -scenario azure and a nonempty connection-string environment reference")
	}
	if selected == "azure" && azureConnectionEnv == "" {
		panic("-scenario azure requires -azure-connection-env")
	}
	if workers < 1 || workers > 256 || duration <= 0 || pollInterval < 0 || sampleInterval < time.Second || settleTimeout < 0 {
		panic("invalid load limits")
	}
	if collectionProfile != "health" && collectionProfile != "journal" && collectionProfile != "standard" && collectionProfile != "diagnostic" {
		panic("invalid collection profile")
	}
	if !strings.Contains("|all|baseline|enabled|outage-recovery|network-faults|disk-full|near-cap|tiny-files|legacy|spool-failure|azure|crash|startup|", "|"+selected+"|") {
		panic("unknown scenario")
	}
	bin, _ = filepath.Abs(bin)
	if out == "" {
		panic("-out required")
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		panic("-out must be a new or empty directory")
	}
	must(os.MkdirAll(out, 0700))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		m := mode.Load()
		modeRequests[m].Add(1)
		if m == 2 {
			select {
			case <-time.After(7 * time.Second):
			case <-r.Context().Done():
				return
			}
		}
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		defer func() { _ = reader.Close() }()
		if selected == "startup" {
			consumeStartupReplay(w, reader, m)
			return
		}
		scan := bufio.NewScanner(reader)
		scan.Buffer(make([]byte, 4096), 2<<20)
		for scan.Scan() {
			encoded.Add(int64(len(scan.Bytes())))
			if m != 0 && m != 5 {
				continue
			}
			var e struct {
				Data struct {
					BaseData struct{ Properties map[string]string }
				}
			}
			if json.Unmarshal(scan.Bytes(), &e) != nil {
				continue
			}
			p := e.Data.BaseData.Properties
			if p["goobers.journal.kind"] == "run" {
				journalKeys.Store(p["goobers.run.id"]+":"+p["goobers.journal.seq"], true)
			}
			received.Add(1)
			if id := p["goobers.telemetry.record_id"]; id != "" {
				if _, old := ids.LoadOrStore(id, true); old {
					duplicates.Add(1)
				}
			}
			streamMu.Lock()
			streams[p["goobers.telemetry.stream"]]++
			streamMu.Unlock()
		}
		if m != 0 {
			rejects.Add(1)
			respondNetworkFault(w, m)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	if selected == "startup" {
		runStartupPairs(server.URL)
		return
	}
	if selected == "crash" {
		root, api := setup("crash", server.URL, true)
		mode.Store(1)
		for _, name := range []string{"crash-outage", "restart-recovery"} {
			r := run(name, root, api, duration/2)
			b, _ := json.MarshalIndent(r, "", "  ")
			write(filepath.Join(out, name+".json"), string(b))
			fmt.Println(string(b))
			validate(r)
			mode.Store(0)
		}
		return
	}
	scenarios := []string{"baseline", "enabled", "outage-recovery", "near-cap", "tiny-files", "legacy", "spool-failure"}
	if selected == "azure" {
		scenarios = []string{"azure"}
	}
	if selected == "network-faults" {
		scenarios = []string{"network-faults"}
	}
	if selected == "disk-full" {
		scenarios = []string{"disk-full"}
	}
	for _, name := range scenarios {
		if selected != "all" && selected != name {
			continue
		}
		mode.Store(0)
		root, api := setup(name, server.URL, name != "baseline")
		if name == "disk-full" {
			// Only the exporter is on the small fault volume; authoritative
			// journals and workflow state remain on the instance filesystem.
			must(os.Symlink(diskFullVolume, filepath.Join(root, "telemetry-export")))
		}
		if name == "network-faults" {
			mode.Store(networkFaultMode(0, duration))
		}
		if name == "spool-failure" {
			write(filepath.Join(root, "telemetry-export"), "synthetic obstruction, not a directory")
		}
		if name == "near-cap" {
			prefill(root, 520, 0, false)
			mode.Store(1)
		}
		if name == "tiny-files" {
			prefill(root, 0, 12000, false)
			mode.Store(1)
		}
		if name == "legacy" {
			prefill(root, 64, 0, true)
			mode.Store(1)
		}
		result := run(name, root, api, duration)
		b, _ := json.MarshalIndent(result, "", "  ")
		write(filepath.Join(out, name+".json"), string(b))
		fmt.Println(string(b))
		validate(result)
	}
}

func validate(r Result) {
	if r.Name == "disk-full" && (!r.DiskFullConfirmed || r.DiskFullBytes == 0) {
		panic("disk-full fixture did not confirm ENOSPC")
	}
	if r.Name == "network-faults" {
		for _, name := range networkModeNames {
			if r.NetworkModes[name] == 0 {
				panic("network phase not exercised: " + name + "; increase duration")
			}
		}
		if r.Duplicates == 0 {
			panic("ambiguous acknowledgement did not exercise duplicate replay")
		}
	}
	if r.Runs == 0 || r.Failures != 0 || r.HealthFailures != 0 || r.MetricSampleErrors != 0 || r.ShutdownMS > 20000 {
		panic("daemon health/workflow/measurement/shutdown invariant failed; inspect result JSON")
	}
	if r.Name != "baseline" && !r.Replay.AccountingReady {
		panic("replay accounting unavailable; build the driver from the candidate source and inspect storage health")
	}
	if r.Name != "baseline" && r.Name != "crash-outage" && collectionProfile != "health" && !r.ExternalReceiver && (r.ExpectedRunEvents == 0 || r.MissingRunEvents != 0 || r.MissingPersistedRecords != 0) {
		panic("journal reconciliation failed; inspect result JSON")
	}
}

func run(name, root, api string, duration time.Duration) Result {
	result := Result{Name: name}
	streamMu.Lock()
	streamStart := map[string]int{}
	for k, v := range streams {
		streamStart[k] = v
	}
	streamMu.Unlock()
	rq, rec, rej, dup := requests.Load(), received.Load(), rejects.Load(), duplicates.Load()
	var modeStart [6]int64
	for i := range modeStart {
		modeStart[i] = modeRequests[i].Load()
	}
	log, err := os.Create(filepath.Join(out, name+"-daemon.log"))
	must(err)
	defer func() { must(log.Close()) }()
	// Keep the normal heartbeat: it captures Go heap/retained memory,
	// goroutines and cgroup CPU/memory pressure without extra instrumentation.
	c := cmd(context.Background(), "up", "--drain-timeout", "15s", root)
	c.Env = append(c.Env, "GODEBUG=gctrace=1")
	c.Stdout = log
	c.Stderr = log
	started := time.Now()
	must(c.Start())
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = c.Process.Kill()
			<-done
		}
	}()
	for {
		select {
		case err := <-done:
			exited = true
			panic(fmt.Sprintf("daemon %s early exit %v; see log", name, err))
		default:
		}
		r, err := client.Get(api + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
			if r.StatusCode == 200 {
				identity, identityErr := client.Get(api + "/api/v1/instance")
				if identityErr == nil {
					_, _ = io.Copy(io.Discard, identity.Body)
					_ = identity.Body.Close()
					if identity.StatusCode == 200 {
						break
					}
				}
			}
		}
		if time.Since(started) > 60*time.Second {
			panic("startup deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}
	result.StartupMS = float64(time.Since(started).Microseconds()) / 1000
	fmt.Printf("START %s pid=%d ready_ms=%.1f duration=%s\n", name, c.Process.Pid, result.StartupMS, duration)
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	samples, err := os.Create(filepath.Join(out, name+"-samples.jsonl"))
	must(err)
	defer func() { must(samples.Close()) }()
	var mu sync.Mutex
	var runLatency, healthLatency []float64
	var cpus []float64
	var sampler processSampler
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			// Normal polling models ten independent polling schedules. Sending
			// ten CLI mutations in the same instant instead tests the API's
			// four-request admission guard (even with telemetry disabled).
			// Zero poll interval deliberately preserves synchronized burst load.
			if pollInterval > 0 && worker > 0 {
				timer := time.NewTimer(time.Duration(worker) * pollInterval / time.Duration(workers))
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
			for ctx.Err() == nil {
				g := "demo"
				if worker%2 == 1 {
					g = "synthetic"
				}
				start := time.Now()
				workctx, stop := context.WithTimeout(context.Background(), 45*time.Second)
				b, err := cmd(workctx, "run", "--force", "--gaggle", g, fmt.Sprintf("load%d", worker%5), root).CombinedOutput()
				stop()
				mu.Lock()
				runLatency = append(runLatency, float64(time.Since(start).Microseconds())/1000)
				if err == nil {
					result.Runs++
				} else {
					result.Failures++
					if result.Failures < 5 {
						write(filepath.Join(out, fmt.Sprintf("%s-failure-%d.txt", name, result.Failures)), string(b))
					}
				}
				mu.Unlock()
				if pollInterval > 0 {
					timer := time.NewTimer(pollInterval)
					select {
					case <-timer.C:
					case <-ctx.Done():
					}
					timer.Stop()
				}
			}
		}(worker)
	}
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()
	phase := time.Now()
	obstructionRepaired := false
	var full *diskFullFixture
	defer func() {
		if full != nil {
			must(full.restore())
		}
	}()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			continue
		case <-ticker.C:
		}
		elapsed := time.Since(phase)
		if name == "disk-full" && full == nil && elapsed >= duration/3 {
			full, err = fillDiskFullVolume(diskFullVolume)
			must(err)
			result.DiskFullConfirmed, result.DiskFullBytes = true, full.bytes
			fmt.Printf("DISK full confirmed=ENOSPC bytes=%d elapsed=%s\n", full.bytes, elapsed.Round(time.Second))
		}
		if full != nil && !full.restored && elapsed >= 2*duration/3 {
			must(full.restore())
			fmt.Printf("DISK restored elapsed=%s\n", elapsed.Round(time.Second))
		}
		if name == "network-faults" {
			next := networkFaultMode(elapsed, duration)
			if mode.Swap(next) != next {
				fmt.Printf("NETWORK phase=%s elapsed=%s\n", networkModeNames[next], elapsed.Round(time.Second))
			}
		}
		if name == "spool-failure" && !obstructionRepaired && elapsed > duration/3 {
			// Remove only the regular-file fixture created above, never a spool tree.
			path := filepath.Join(root, "telemetry-export")
			info, err := os.Lstat(path)
			must(err)
			if !info.Mode().IsRegular() {
				panic("spool obstruction fixture changed unexpectedly")
			}
			must(os.Remove(path))
			obstructionRepaired = true
		}
		if name == "outage-recovery" {
			if elapsed < duration/3 {
				mode.Store(0)
			} else if elapsed < 2*duration/3 {
				mode.Store(1)
			} else {
				mode.Store(0)
			}
		}
		if name == "near-cap" || name == "tiny-files" || name == "legacy" {
			recovery := duration / 2
			if recoveryAfter > 0 {
				recovery = recoveryAfter
			}
			if elapsed > recovery {
				mode.Store(0)
			}
		}
		if name == "outage-recovery" && elapsed > duration/2 && elapsed < 2*duration/3 {
			mode.Store(2)
		}
		start := time.Now()
		r, err := client.Get(api + "/api/v1/health")
		ms := float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			result.HealthFailures++
		} else {
			b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			_ = r.Body.Close()
			if r.StatusCode != 200 {
				result.HealthFailures++
			}
			if int(elapsed.Seconds())%15 == 0 {
				write(filepath.Join(out, name+"-health-latest.json"), string(b))
			}
		}
		healthLatency = append(healthLatency, ms)
		rss, cpu, sampleErr := sampler.sample(c.Process.Pid)
		if sampleErr != nil {
			result.MetricSampleErrors++
		}
		result.MaxRSSKiB = max(result.MaxRSSKiB, rss)
		cpus = append(cpus, cpu)
		stat := telemetry.InspectAzureReplayRoot(spool(root))
		b, _ := json.Marshal(map[string]any{"elapsed": elapsed.Seconds(), "rssKiB": rss, "cpu": cpu, "openHandlesOrFDs": sampler.handles, "healthMS": ms, "mode": mode.Load(), "replay": stat})
		_, err = fmt.Fprintln(samples, string(b))
		must(err)
		if int(elapsed.Seconds())%15 == 0 {
			mu.Lock()
			fmt.Printf("%s t=%.0fs runs=%d failed=%d rss=%dKiB cpu=%.1f accounting=%t spool=%dB records=%d\n", name, elapsed.Seconds(), result.Runs, result.Failures, rss, cpu, stat.AccountingReady, stat.PendingBytes, stat.PendingRecords)
			mu.Unlock()
		}
	}
	wg.Wait()
	if name != "crash-outage" {
		mode.Store(0)
	}
	if name != "baseline" && name != "crash-outage" && collectionProfile != "health" && azureConnectionEnv == "" {
		started := time.Now()
		keys := expectedJournalKeys(root)
		deadline := started.Add(settleTimeout)
		for time.Now().Before(deadline) {
			missing := 0
			for _, key := range keys {
				if _, ok := journalKeys.Load(key); !ok {
					missing++
				}
			}
			if missing == 0 {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		result.CatchupWaitMS = float64(time.Since(started).Microseconds()) / 1000
	}
	result.Replay = telemetry.InspectAzureReplayRoot(spool(root))
	result.RunP50MS = percentile(runLatency, .5)
	result.RunP95MS = percentile(runLatency, .95)
	result.HealthP95MS = percentile(healthLatency, .95)
	for _, v := range cpus {
		result.MeanCPU += v
	}
	result.MeanCPU /= float64(max(1, len(cpus)))
	start := time.Now()
	if name == "crash-outage" {
		paths, _ := filepath.Glob(filepath.Join(spool(root), "*", "*.ndjson"))
		for _, path := range paths {
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			scan := bufio.NewScanner(f)
			scan.Buffer(make([]byte, 4096), 2<<20)
			for scan.Scan() {
				var e struct {
					Data struct {
						BaseData struct{ Properties map[string]string }
					}
				}
				if json.Unmarshal(scan.Bytes(), &e) == nil {
					if id := e.Data.BaseData.Properties["goobers.telemetry.record_id"]; id != "" {
						checkpointIDs = append(checkpointIDs, id)
					}
				}
			}
			must(scan.Err())
			must(f.Close())
		}
		b, _ := json.Marshal(checkpointIDs)
		write(filepath.Join(out, "prekill-spool-record-ids.json"), string(b))
		must(c.Process.Kill())
	} else {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		stopOutput, stopErr := cmd(stopCtx, "down", root).CombinedOutput()
		stopCancel()
		if stopErr != nil {
			panic(fmt.Sprintf("request shutdown: %v: %s", stopErr, stopOutput))
		}
	}
	select {
	case err := <-done:
		exited = true
		if err != nil {
			fmt.Printf("shutdown error: %v\n", err)
		}
	case <-time.After(25 * time.Second):
		panic("shutdown deadline")
	}
	result.ShutdownMS = float64(time.Since(start).Microseconds()) / 1000
	result.CPUSeconds = c.ProcessState.UserTime().Seconds() + c.ProcessState.SystemTime().Seconds()
	result.WallSeconds = time.Since(started).Seconds()
	result.Requests = requests.Load() - rq
	result.NetworkModes = make(map[string]int64, len(modeRequests))
	for i := range modeRequests {
		result.NetworkModes[networkModeNames[i]] = modeRequests[i].Load() - modeStart[i]
	}
	result.Records = received.Load() - rec
	result.Rejected = rejects.Load() - rej
	result.Duplicates = duplicates.Load() - dup
	streamMu.Lock()
	result.Streams = map[string]int{}
	for k, v := range streams {
		result.Streams[k] = v - streamStart[k]
	}
	streamMu.Unlock()
	result.DiskKiB = logicalDiskKiB(root)
	paths, _ := filepath.Glob(filepath.Join(root, "gaggles", "*", "runs", "*", "events.jsonl"))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 4096), 2<<20)
		id := filepath.Base(filepath.Dir(path))
		for scan.Scan() {
			var e struct{ Seq uint64 }
			if json.Unmarshal(scan.Bytes(), &e) == nil {
				result.ExpectedRunEvents++
				if _, ok := journalKeys.Load(fmt.Sprintf("%s:%d", id, e.Seq)); !ok {
					result.MissingRunEvents++
					if azureConnectionEnv == "" && name != "baseline" && name != "crash-outage" && collectionProfile != "health" && result.MissingRunEvents <= 20 {
						fmt.Printf("MISSING %s:%d\n", id, e.Seq)
					}
				}
			}
		}
		must(scan.Err())
		must(f.Close())
	}
	result.PersistedCheckpointRecords = len(checkpointIDs)
	for _, id := range checkpointIDs {
		if _, ok := ids.Load(id); !ok {
			result.MissingPersistedRecords++
		}
	}
	if azureConnectionEnv != "" {
		result.ExternalReceiver = true
		result.MissingRunEvents = -1 // Requires separate Azure query reconciliation.
	}
	return result
}

func logicalDiskKiB(root string) int64 {
	var total int64
	must(filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	}))
	return (total + 1023) / 1024
}

func expectedJournalKeys(root string) []string {
	paths, err := filepath.Glob(filepath.Join(root, "gaggles", "*", "runs", "*", "events.jsonl"))
	must(err)
	var keys []string
	for _, path := range paths {
		f, err := os.Open(path)
		must(err)
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 4096), 8<<20)
		for scan.Scan() {
			var event struct{ Seq uint64 }
			must(json.Unmarshal(scan.Bytes(), &event))
			keys = append(keys, fmt.Sprintf("%s:%d", filepath.Base(filepath.Dir(path)), event.Seq))
		}
		must(scan.Err())
		must(f.Close())
	}
	return keys
}
