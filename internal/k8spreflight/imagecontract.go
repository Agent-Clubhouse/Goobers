package k8spreflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func inspectOverlayImages(ctx context.Context, opts Options) (int, []string, error) {
	pins, err := collectOverlayPins(opts.OverlayDir)
	if err != nil {
		return 0, nil, err
	}
	if status, detail := compareOverlayPins(pins); status != StatusPass {
		return 0, nil, errors.New(detail)
	}
	runtime := opts.ImageRuntime
	if runtime == "" {
		runtime = "docker"
	}
	if runtime != "docker" && runtime != "podman" {
		return 0, nil, fmt.Errorf("image runtime must be docker or podman")
	}
	rootCA, err := readImageRootCA(opts.ImageCAFile)
	if err != nil {
		return 0, nil, err
	}
	run := opts.runOverlayCommand
	if run == nil {
		run = executeOverlayCommand
	}
	boundedRun := func(parent context.Context, command overlayCommand) ([]byte, error) {
		probeCtx, cancel := context.WithTimeout(parent, opts.timeout())
		defer cancel()
		return run(probeCtx, command)
	}
	rendered, err := boundedRun(ctx, overlayCommand{Program: "kubectl", Args: []string{"kustomize", opts.OverlayDir}})
	if err != nil {
		return 0, nil, fmt.Errorf("render consumer overlay: %w", err)
	}
	requirements, err := renderedImageRequirements(rendered, pins, opts.ImageTools, rootCA)
	if err != nil {
		return 0, nil, err
	}
	if len(requirements) == 0 {
		return 0, nil, fmt.Errorf("render contains no pinned Goobers container images (checked 0)")
	}
	checked := 0
	var unchecked, failures []string
	for _, required := range requirements {
		required.PullPolicy = opts.ImagePullPolicy
		observation, err := probePinnedImage(ctx, boundedRun, runtime, required)
		checked += observation.Checked
		unchecked = append(unchecked, observation.Unchecked...)
		if err != nil {
			failures = append(failures, required.Image+": "+err.Error())
		}
	}
	if len(failures) != 0 {
		return checked, unchecked, errors.New(strings.Join(failures, "; "))
	}
	return checked, slices.Compact(unchecked), nil
}

func readImageRootCA(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat internal root CA: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("internal root CA must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read internal root CA: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("internal root CA must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, fmt.Errorf("cannot read internal root CA within 64 KiB")
	}
	return data, nil
}

func renderedImageRequirements(data []byte, pins []overlayPin, tools []string, rootCA []byte) ([]imageRequirements, error) {
	if len(pins) == 0 {
		return nil, fmt.Errorf("no source pins inspected (checked 0)")
	}
	known := map[string]bool{}
	for _, pin := range pins {
		if pin.Image != "" {
			known[pin.Image] = true
		}
	}
	images := map[string]*imageRequirements{}
	for _, pin := range pins {
		if pin.Kind == "host" {
			images[pin.Image] = &imageRequirements{Image: pin.Image}
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse rendered overlay: %w", err)
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		switch nodeValue(root, "kind") {
		case "Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "ReplicaSet", "ReplicationController":
			if err := collectRenderedContainers(root, known, images); err != nil {
				return nil, err
			}
		}
	}
	if len(images) > 128 {
		return nil, fmt.Errorf("render exceeds the 128-image inspection bound")
	}
	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)
	var result []imageRequirements
	for _, name := range names {
		required := images[name]
		required.Commit, required.RootCA = pins[0].Commit, rootCA
		required.Tools = append(required.Tools, tools...)
		sort.Strings(required.Tools)
		required.Tools = slices.Compact(required.Tools)
		sort.Strings(required.ScriptVariables)
		required.ScriptVariables = slices.Compact(required.ScriptVariables)
		sort.Strings(required.Executables)
		required.Executables = slices.Compact(required.Executables)
		result = append(result, *required)
	}
	return result, nil
}

func collectRenderedContainers(node *yaml.Node, known map[string]bool, images map[string]*imageRequirements) error {
	if node.Kind == yaml.AliasNode {
		return fmt.Errorf("rendered YAML aliases cannot be inspected safely")
	}
	if node.Kind == yaml.MappingNode {
		for _, key := range []string{"containers", "initContainers"} {
			if containers := nodeField(node, key); containers != nil {
				if containers.Kind != yaml.SequenceNode {
					return fmt.Errorf("rendered %s is not a sequence", key)
				}
				for _, container := range containers.Content {
					image := nodeValue(container, "image")
					if !known[image] && !goobersImage(image) {
						continue
					}
					required := images[image]
					if required == nil {
						required = &imageRequirements{Image: image}
						images[image] = required
					}
					if err := collectContainerRequirements(container, required); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, child := range node.Content {
		if err := collectRenderedContainers(child, known, images); err != nil {
			return err
		}
	}
	return nil
}
