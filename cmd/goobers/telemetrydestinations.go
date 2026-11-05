package main

import (
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/instance"
)

func telemetryTestConnection(source instance.TelemetryConfig, name string) (instance.TokenRef, error) {
	return bootstrap.TelemetryTestConnection(source, name)
}
