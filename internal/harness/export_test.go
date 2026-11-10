package harness

// Test-only exports for the external harness_test package.
var (
	LoadBenchmarkTestFixture = loadBenchmarkTestFixture
	BenchmarkRunRequest      = benchmarkRunRequest
	BenchmarkErrorDetail     = benchmarkErrorDetail
	TriageAct                = triageAct
	RunBenchmark             = runBenchmark
)
