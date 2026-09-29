package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

type startupConfig struct {
	Rounds         int
	Prefill, Index string
	Endpoint       string
	Settle         time.Duration
	PostReady      time.Duration
}

var startupOptions = startupConfig{Rounds: 20, Prefill: "empty", Index: "cold", Endpoint: "healthy", Settle: 5 * time.Second}

func registerStartupFlags() {
	flag.IntVar(&startupOptions.Rounds, "startup-rounds", 20, "matched startup pairs; >=20 recommended for p95, max100")
	flag.StringVar(&startupOptions.Prefill, "startup-prefill", "empty", "startup only: empty, half, near-cap, cap, tiny-files, legacy")
	flag.StringVar(&startupOptions.Index, "startup-index", "cold", "startup only: cold (missing manifest) or warm (offline primed)")
	flag.StringVar(&startupOptions.Endpoint, "startup-endpoint", "healthy", "startup only: healthy or stalled (seven-second response delay)")
	flag.DurationVar(&startupOptions.Settle, "startup-settle", 5*time.Second, "idle time after fixture preparation, before each startup measurement")
	flag.DurationVar(&startupOptions.PostReady, "startup-post-ready", 0, "optional post-ready API sampling window (up to 1m), with one real workflow; zero preserves idle startup measurements")
}

func (c startupConfig) validate(selected string) error {
	if selected != "startup" {
		var invalid bool
		flag.Visit(func(f *flag.Flag) { invalid = invalid || strings.HasPrefix(f.Name, "startup-") })
		if invalid {
			return fmt.Errorf("startup flags require -scenario startup")
		}
		return nil
	}
	if c.Rounds < 1 || c.Rounds > 100 || c.Settle < 0 || c.Settle > time.Minute || c.PostReady < 0 || c.PostReady > time.Minute {
		return fmt.Errorf("invalid startup repetitions or settle duration")
	}
	if c.Index != "cold" && c.Index != "warm" {
		return fmt.Errorf("startup index must be cold or warm")
	}
	if c.Endpoint != "healthy" && c.Endpoint != "stalled" {
		return fmt.Errorf("startup endpoint must be healthy or stalled")
	}
	_, _, _, err := startupPrefill(c.Prefill)
	return err
}

func startupPrefill(name string) (mib, files int, legacy bool, err error) {
	switch name {
	case "empty":
	case "half":
		mib = 256
	case "near-cap":
		mib = 460
	case "cap":
		mib = 512
	case "tiny-files":
		files = 12000
	case "legacy":
		mib, legacy = 64, true
	default:
		err = fmt.Errorf("unknown startup prefill %q", name)
	}
	return
}

type startupOccupancy struct {
	Files, Bytes int64
	Manifest     bool
}

// Count actual payload bytes without opening the manifest or initializing an
// exporter. In particular, never warm a cold index just to measure its size.
func startupDiskState(root string) (startupOccupancy, error) {
	var state startupOccupancy
	err := filepath.WalkDir(spool(root), func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) && path == spool(root) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected startup fixture symlink: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.Name() == ".replay-index.db" {
			state.Manifest = info.Size() > 0
		}
		if strings.HasSuffix(entry.Name(), ".ndjson") {
			state.Files++
			state.Bytes += info.Size()
		}
		return nil
	})
	return state, err
}

type startupSample struct {
	Round                        int
	Enabled                      bool
	Name                         string
	Before, After                startupOccupancy
	Measurement                  startupTiming
	Prime                        *startupTiming `json:",omitempty"`
	SeedPayloadsRemovedAfterward bool
}

func runStartupPairs(url string) {
	if _, present := os.LookupEnv("GOOBERS_DISABLE_FSYNC"); present {
		panic("startup measurements require absent GOOBERS_DISABLE_FSYNC")
	}
	samples := make([]startupSample, 0, 2*startupOptions.Rounds)
	for round := 1; round <= startupOptions.Rounds; round++ {
		order := []bool{false, true}
		if round%2 == 0 {
			order = []bool{true, false}
		}
		for _, enabled := range order {
			sample := runStartupSample(url, round, enabled)
			samples = append(samples, sample)
			data, err := json.MarshalIndent(struct {
				Config              startupConfig
				Samples             []startupSample
				Complete            bool
				OSCacheCold         bool
				FsyncOverrideAbsent bool
			}{startupOptions, samples, len(samples) == 2*startupOptions.Rounds, false, true}, "", "  ")
			must(err)
			write(filepath.Join(out, "startup-results.json"), string(data))
			fmt.Printf("STARTUP round=%d enabled=%v ready_ms=%.3f stop_ms=%.3f files=%d bytes=%d\n", round, enabled, sample.Measurement.StartupMS, sample.Measurement.ShutdownMS, sample.Before.Files, sample.Before.Bytes)
			if enabled && sample.Measurement.Requests == 0 {
				panic("startup fixture did not exercise the configured ingestion endpoint; inspect raw sample")
			}
			if post := sample.Measurement.PostReady; post != nil && !post.Successful() {
				panic("post-ready response/workflow failure; raw startup sample retained")
			}
		}
	}
}

func runStartupSample(url string, round int, enabled bool) startupSample {
	name := fmt.Sprintf("startup-%03d-%v", round, enabled)
	root, api := setup(name, url, enabled)
	marker := filepath.Join(root, ".startup-fixture")
	write(marker, "synthetic-startup-seeds-v1\n")
	mib, files, legacy, err := startupPrefill(startupOptions.Prefill)
	must(err)
	prefill(root, mib, files, legacy)
	must(visitStartupSeeds(root, false)) // Sync preparation writes before timing.
	sample := startupSample{Round: round, Enabled: enabled, Name: name}
	if startupOptions.Index == "warm" {
		mode.Store(1) // Build accounting without draining the seeded backlog.
		prime := measureStartup(name+"-prime", root, api, enabled, 0)
		sample.Prime = &prime
		if enabled && !telemetry.InspectAzureReplayRoot(spool(root)).AccountingReady {
			panic("warm startup fixture has no usable persisted replay accounting")
		}
	}
	sample.Before, err = startupDiskState(root)
	must(err)
	if startupOptions.Index == "cold" && sample.Before.Manifest {
		panic("cold startup fixture already has an index")
	}
	if enabled && startupOptions.Index == "warm" && !sample.Before.Manifest {
		panic("warm startup fixture has no manifest")
	}
	if enabled && startupOptions.Index == "warm" {
		must(validateStartupWarm(sample.Before, telemetry.InspectAzureReplayRoot(spool(root))))
	}
	mode.Store(0)
	if startupOptions.Endpoint == "stalled" {
		mode.Store(2)
	}
	time.Sleep(startupOptions.Settle)
	sample.Measurement = measureStartup(name, root, api, false, startupOptions.PostReady)
	sample.After, err = startupDiskState(root)
	must(err)
	// Only the known, synthetic seed payload files are removed, after the child
	// has exited and pre/post occupancy was captured. Keep journals, manifest,
	// logs and timing evidence. This bounds fixture disk growth across repeats;
	// these post-cleanup fixtures cannot certify replay delivery completeness.
	must(visitStartupSeeds(root, true))
	sample.SeedPayloadsRemovedAfterward = true
	return sample
}

func validateStartupWarm(state startupOccupancy, stats telemetry.AzureReplayStats) error {
	if !state.Manifest || !stats.AccountingReady || int64(stats.PendingFiles) != state.Files || stats.PendingBytes != state.Bytes {
		return fmt.Errorf("warm startup manifest does not match the stopped fixture: disk=%+v indexed=%+v", state, stats)
	}
	return nil
}
