package main

import (
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
)

func trustedLocalChildren(cfg *instance.Config) bool {
	return childworkflow.UsesTrustedLocalExecution(cfg)
}
