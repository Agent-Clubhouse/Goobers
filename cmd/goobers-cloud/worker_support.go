package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
)

var loadConfigDirectory = instance.LoadConfigDir
var diagnosticsMode bool

const diagnosticsMaxOutputBytes int64 = 64 << 20

func prepareManualRoot(layout instance.Layout, diagnostic io.Writer) error {
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		return fmt.Errorf("%s not found (not an instance root)", layout.ConfigFile())
	}
	if err := instance.RequireCurrentRoot(layout.Root); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := instance.EnsureRootIdentity(ctx, layout.Root)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(layout.Root)
	if err != nil {
		absolute = filepath.Clean(layout.Root)
	}
	_, err = fmt.Fprintf(diagnostic, "Instance root: %q; instance ID: %q\n", absolute, id)
	return err
}

func validationIssueSummary(report *validate.Report) string {
	if report == nil {
		return ""
	}
	var messages []string
	for _, issue := range report.Issues {
		if issue.Severity == validate.Error {
			messages = append(messages, issue.String())
		}
	}
	sort.Strings(messages)
	return fmt.Sprint(messages)
}

func validateSeededWorkerConfig(root string) error {
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return err
	}
	if config.WorkflowSource != nil || config.ConfigMirrorPath != "" {
		return fmt.Errorf("worker seed must not fetch config sources or publish mirrors")
	}
	set, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		return fmt.Errorf("seeded worker config is invalid: %w (%s)", err, validationIssueSummary(report))
	}
	if _, _, err := runtimewiring.LoadSnapshotGooberInputs(layout.ConfigDir(), set); err != nil {
		return err
	}
	_, err = runtimewiring.ConfigDirectoryDigest(layout.ConfigDir())
	return err
}
