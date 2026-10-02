# cmd/goobers growth: PR #6085

#6073. Adds the opt-in decisionGate shadow observer wiring for agentic stage results (decisionshadow.go and the runner wiring). Off by default and a no-op unless decisionGate.mode is set; the decision logic lives in internal/decisiongate and only the instance-config wiring belongs in cmd/goobers.
