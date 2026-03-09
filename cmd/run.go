package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"spark-cli/internal/cloud"
	"spark-cli/internal/config"
	"spark-cli/internal/events"
	"spark-cli/internal/events/subscribers"
	"spark-cli/internal/finder"
	"spark-cli/internal/model"
	"spark-cli/internal/output"
	"spark-cli/internal/parser"
	"spark-cli/internal/runner"
	"spark-cli/internal/telemetry"
)

var junitOutput string
var htmlOutput string
var artifactsDir string
var regenerateSnapshots bool
var filterTags []string
var filterName string
var verboseOutput bool
var debugOutput bool
var testTimeout int
var workerCount int
var cloudMode bool
var cpuThreshold float64
var teamcityOutput bool
var jsonOutput bool
var pathMapping string
var configFile string

var runCmd = &cobra.Command{
	Use:   "run [path]",
	Short: "Run tests from spark.yaml files",
	Long: `Run discovers and executes tests defined in *.spark files.

The command walks through the specified directory (or current directory if not specified)
and finds all files matching the *.spark pattern. You can also specify a single
.spark file directly.

Use --tags to run only tests with specific tags. Multiple tags can be specified
(comma-separated or multiple flags). Tests matching ANY of the specified tags will run.

Use --filter to run only tests whose name matches the pattern. Supports glob patterns
with * wildcard. Without wildcards, performs substring match.

Use --configuration to specify a spark.yaml configuration file with shared service
definitions. If not specified, spark.yaml/spark.yml is auto-discovered in the
current working directory.

Output modes:
  --verbose    Show detailed logs (all events in logfmt format)
  --debug      Show docker commands being executed (implies --verbose)

Example:
  spark run ./tests
  spark run ./tests/auth.spark
  spark run .
  spark run
  spark run ./tests --tags smoke
  spark run ./tests --tags smoke,api
  spark run ./tests --filter "login*"
  spark run ./tests --filter "*authentication*"
  spark run ./tests --tags smoke --filter "user*"
  spark run ./tests --verbose
  spark run ./tests --debug
  spark run ./tests --configuration spark.yaml
  spark run ./tests --cloud
  SPARK_CLOUD_URL=http://localhost:8080 spark run ./tests --cloud`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true, // Don't print usage on error - it's confusing in CI logs
	SilenceErrors: true,
	RunE:          runTests,
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().StringVar(&junitOutput, "junit", "", "Write JUnit XML report to file")
	runCmd.Flags().StringVar(&htmlOutput, "html", "", "Write HTML reports to directory")
	runCmd.Flags().StringVar(&artifactsDir, "artifacts", "./artifacts", "Directory for test artifacts")
	runCmd.Flags().BoolVar(&regenerateSnapshots, "regenerate-snapshots", false, "Update snapshot files instead of comparing them")
	runCmd.Flags().StringSliceVar(&filterTags, "tags", nil, "Run only tests with specified tags (comma-separated or multiple flags)")
	runCmd.Flags().StringVar(&filterName, "filter", "", "Run only tests whose name matches the pattern (supports * wildcard)")
	runCmd.Flags().BoolVar(&verboseOutput, "verbose", false, "Show detailed logs (all events)")
	runCmd.Flags().BoolVar(&debugOutput, "debug", false, "Show docker commands (implies --verbose)")
	runCmd.Flags().IntVar(&testTimeout, "timeout", 300, "Maximum time in seconds for each test execution")
	runCmd.Flags().IntVar(&workerCount, "workers", 0, "Number of parallel test workers (0 = auto-detect from CPU count)")
	runCmd.Flags().BoolVar(&cloudMode, "cloud", false, "Run tests on remote API server (default: https://spark-cloud.finie.io, override with SPARK_CLOUD_URL)")
	runCmd.Flags().Float64Var(&cpuThreshold, "cpu-threshold", 0, "CPU load threshold (0.0-1.0) - pause test execution when exceeded (0 = disabled)")
	runCmd.Flags().BoolVar(&teamcityOutput, "teamcity", false, "Output TeamCity service messages for IDE integration")
	runCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output NDJSON for machine consumption")
	runCmd.Flags().StringVar(&pathMapping, "path-mapping", "", "Path prefix mapping for artifact paths (container=host, e.g. /srv/spark=/Users/me/project)")
	runCmd.Flags().StringVar(&configFile, "configuration", "", "Path to spark.yaml configuration file (auto-discovered if not set)")
}

// runTests is the main entry point for the run command.
func runTests(cmd *cobra.Command, args []string) error {
	// Determine the search path
	searchPath := "."
	if len(args) > 0 {
		searchPath = args[0]
	}

	// Verify path exists
	info, err := os.Stat(searchPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("path does not exist: %s", searchPath)
	}

	// Find test files - either single file or directory search
	var files []string
	if !info.IsDir() && strings.HasSuffix(searchPath, ".spark") {
		// Single file specified
		files = []string{searchPath}
	} else {
		// Directory search
		f := finder.New(searchPath)
		files, err = f.FindTestFiles()
		if err != nil {
			return fmt.Errorf("failed to find test files: %w", err)
		}
	}

	if len(files) == 0 {
		fmt.Printf("No *.spark files found in %s\n", searchPath)
		return nil
	}

	// Parse all found files
	p := parser.New()
	parseResult := p.ParseAll(files)

	// Report any parse errors
	for _, err := range parseResult.Errors {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}

	// Load configuration file
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	services := cfg.ServiceTemplates()

	// Show telemetry notice on first run
	telemetry.ShowNoticeIfNeeded(teamcityOutput || jsonOutput)
	startTime := time.Now()

	// Create event bus and subscribers
	bus := events.NewBus()
	collector := subscribers.NewEventCollector()
	collector.Register(bus)

	// debug implies verbose
	if debugOutput {
		verboseOutput = true
	}

	// Set up output based on flags
	if teamcityOutput {
		// TeamCity mode: output service messages for IDE test runner
		tc := subscribers.NewTeamCityReporter(os.Stdout, artifactsDir, pathMapping, htmlOutput)
		tc.Register(bus)
	} else if jsonOutput {
		// JSON mode: NDJSON output for machine consumption
		jr := subscribers.NewJSONReporter(os.Stdout)
		jr.Register(bus)
	} else if verboseOutput {
		// Verbose mode: show all events in logfmt format
		verbose := subscribers.NewVerboseLogger(os.Stdout)
		verbose.Register(bus)

		if debugOutput {
			// Debug mode: also show docker commands
			debug := subscribers.NewDebugLogger(os.Stdout)
			debug.Register(bus)
		}
	} else {
		// Default mode: user-friendly progress output
		cli := subscribers.NewCLIReporter(os.Stdout)
		cli.Register(bus)
	}

	// Create emitter for local logging
	emitter := events.NewEmitter(bus)

	// Log service template discovery
	if services.HasTemplates() {
		emitter.Info(events.Fields{
			"action": "service_discover",
			"count":  fmt.Sprintf("%d", len(services.Templates)),
			"msg":    fmt.Sprintf("Found %d service template(s)", len(services.Templates)),
		})
	}

	// Filter tests by tags if specified
	tests := parseResult.Tests
	if len(filterTags) > 0 {
		tests = tests.FilterByTags(filterTags)
		emitter.Info(events.Fields{
			"action": "tag_filter",
			"tags":   strings.Join(filterTags, ","),
			"count":  fmt.Sprintf("%d", tests.TotalTests()),
			"msg":    fmt.Sprintf("Filtered to %d test(s) matching tags: %s", tests.TotalTests(), strings.Join(filterTags, ", ")),
		})
		if tests.TotalTests() == 0 {
			fmt.Println("No tests match the specified tags")
			return nil
		}
	}

	// Filter tests by name pattern if specified
	if filterName != "" {
		tests = tests.FilterByName(filterName)
		emitter.Info(events.Fields{
			"action":  "name_filter",
			"pattern": filterName,
			"count":   fmt.Sprintf("%d", tests.TotalTests()),
			"msg":     fmt.Sprintf("Filtered to %d test(s) matching pattern: %s", tests.TotalTests(), filterName),
		})
		if tests.TotalTests() == 0 {
			fmt.Println("No tests match the specified filter pattern")
			return nil
		}
	}

	// Warn about flags that have no effect in cloud mode
	if cloudMode {
		ignoredInCloud := []string{
			"html", "junit", "artifacts", "regenerate-snapshots",
			"verbose", "debug", "timeout", "workers", "json",
		}
		for _, flag := range ignoredInCloud {
			if cmd.Flags().Changed(flag) {
				fmt.Fprintf(os.Stderr, "Warning: --%s has no effect in cloud mode\n", flag)
			}
		}
	}

	// Cloud mode: upload tests to remote API and stream results
	if cloudMode {
		cloudURL := os.Getenv("SPARK_CLOUD_URL")
		if cloudURL == "" {
			cloudURL = "https://spark-cloud.finie.io"
		}
		err := runTestsCloud(cloudURL, tests, services, startTime)
		telemetry.Wait(4 * time.Second)
		return err // runTestsCloud already wraps with ExitError
	}

	// Create report writers if output requested (needs to track time from start)
	var junitWriter *output.JUnitWriter
	if junitOutput != "" {
		junitWriter = output.NewJUnitWriter()
	}

	var htmlWriter *output.HTMLWriter
	if htmlOutput != "" {
		if err := os.RemoveAll(htmlOutput); err != nil {
			return fmt.Errorf("failed to clean HTML output directory: %w", err)
		}
		if err := os.MkdirAll(htmlOutput, 0o755); err != nil {
			return fmt.Errorf("failed to create HTML output directory: %w", err)
		}
		htmlWriter = output.NewHTMLWriter()
	}

	// Auto-detect worker count from CPU count if not specified
	if workerCount <= 0 {
		workerCount = runtime.NumCPU()
	}
	if workerCount < 1 {
		workerCount = 1
	}

	// Set up context with Ctrl+C handler for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Fprintf(os.Stderr, "\nInterrupted, cleaning up containers...\n")
		cancel()
		<-sigCh
		fmt.Fprintf(os.Stderr, "\nForce quit\n")
		os.Exit(1)
	}()
	defer signal.Stop(sigCh)

	// Run tests and output results
	r, err := runner.New(bus, workerCount, artifactsDir, services, regenerateSnapshots, testTimeout, Version, collector, cpuThreshold, cfg.ExecutionVariables)
	if err != nil {
		telemetry.RecordError(Version, telemetry.ClassifyError(err))
		telemetry.Wait(4 * time.Second)
		return fmt.Errorf("failed to create test runner: %w", err)
	}

	// Set per-test HTML callback so each test gets its own HTML file
	// immediately after completion (before the TeamCity event fires).
	if htmlWriter != nil {
		htmlDir := htmlOutput
		hw := htmlWriter
		r.SetOnTestComplete(func(result *model.TestResult, suiteName, suiteFilePath string) {
			if _, err := hw.WriteTestReport(result, suiteName, suiteFilePath, htmlDir); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to write test report for %s: %v\n", result.Test.Name, err)
			}
		})
	}

	result := r.Run(ctx, tests)

	// Record telemetry
	telemetry.RecordRun(telemetry.RunParams{
		Version:        Version,
		DurationMs:     time.Since(startTime).Milliseconds(),
		WorkerCount:    workerCount,
		HasHTMLReport:  htmlOutput != "",
		HasJUnitReport: junitOutput != "",
		HasArtifacts:   artifactsDir != "./artifacts",
		HasTagsFilter:  len(filterTags) > 0,
		HasNameFilter:  filterName != "",
	}, result.TotalTests(), result.TotalPassed(), result.TotalFailed(), result.TotalSkipped(), len(tests.Suites))

	// Get weblink prefix for CLI output (e.g., "http://localhost:8080/reports")
	weblink := os.Getenv("SPARK_WEBLINK")
	if weblink != "" {
		weblink = strings.TrimSuffix(weblink, "/")
	}

	// Write JUnit report if requested
	if junitWriter != nil {
		if err := junitWriter.Write(result, junitOutput); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write JUnit report: %v\n", err)
		} else {
			fields := events.Fields{
				"action": "junit_write",
				"target": junitOutput,
				"msg":    "JUnit report written",
			}
			if weblink != "" {
				fields["url"] = weblink + "/" + filepath.Base(junitOutput)
			}
			emitter.Info(fields)
		}
	}

	// Write HTML dashboard (index.html with links to per-test reports)
	if htmlWriter != nil {
		if err := htmlWriter.WriteDashboard(result, htmlOutput, Version); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write HTML dashboard: %v\n", err)
		} else {
			target := filepath.Join(htmlOutput, "index.html")
			fields := events.Fields{
				"action": "html_write",
				"target": target,
				"msg":    "HTML report written",
			}
			if weblink != "" {
				fields["url"] = weblink + "/" + filepath.Base(htmlOutput) + "/index.html"
			}
			emitter.Info(fields)
		}
	}

	// Ensure all TeamCity service messages are flushed before process exits.
	// Docker relay may buffer stdout; sync forces kernel flush so IDE receives
	// testFinished/testSuiteFinished before processTerminated fires.
	if teamcityOutput || jsonOutput {
		os.Stdout.Sync()
	}

	// Wait for telemetry to flush before exit
	telemetry.Wait(4 * time.Second)

	// Determine exit code based on test results:
	//   exit 2 = infrastructure errors (service startup, healthcheck, setup, Docker failures)
	//   exit 1 = assertion failures (tests ran but assertions didn't pass)
	if result.TotalErrors() > 0 {
		return exitErrorf(ExitInfraError, "test run failed: %d test(s) errored, %d failed",
			result.TotalErrors(), result.TotalFailed()-result.TotalErrors())
	}
	if result.HasFailures() {
		return exitErrorf(ExitTestFailure, "test run failed: %d test(s) failed", result.TotalFailed())
	}

	return nil
}

// loadConfig loads the configuration file from --configuration flag or auto-discovers it.
func loadConfig() (*config.Config, error) {
	if configFile != "" {
		cfg, err := config.Load(configFile)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	cfg, found := config.Discover()
	if !found {
		fmt.Fprintln(os.Stderr, "Starting without configuration file")
	}
	return cfg, nil
}

// resolveCloudToken resolves the auth token for cloud mode.
// Priority: SPARK_TOKEN env var > ~/.spark/auth.json > empty string.
func resolveCloudToken(apiURL string) string {
	// 1. Environment variable takes priority
	if token := os.Getenv("SPARK_TOKEN"); token != "" {
		return token
	}

	// 2. Try ./auth.json in current directory
	data, err := os.ReadFile("auth.json")
	if err != nil {
		return ""
	}

	var authFile struct {
		SparkToken map[string]string `json:"spark-token"`
	}
	if err := json.Unmarshal(data, &authFile); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auth.json is not valid JSON, ignoring\n")
		return ""
	}

	if authFile.SparkToken == nil {
		return ""
	}

	// Extract host from apiURL
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return ""
	}

	if token, ok := authFile.SparkToken[parsed.Host]; ok {
		return token
	}

	return ""
}

// runTestsCloud uploads tests to a remote API server and streams results.
func runTestsCloud(apiURL string, tests *model.TestCollection, services *model.ServiceTemplateCollection, startTime time.Time) error {
	token := resolveCloudToken(apiURL)
	client := cloud.NewClient(apiURL, token)

	// Health check
	if err := client.HealthCheck(); err != nil {
		telemetry.RecordError(Version, telemetry.ClassifyError(err))
		return fmt.Errorf("cloud API not reachable at %s: %w\n\nHint: run without --cloud for local execution", apiURL, err)
	}

	// Build submission with resolved service templates
	submission, err := cloud.BuildSubmission(tests, services, Version)
	if err != nil {
		telemetry.RecordError(Version, telemetry.ClassifyError(err))
		return fmt.Errorf("failed to build submission: %w", err)
	}

	fmt.Printf("Uploading %d tests to %s...\n", tests.TotalTests(), apiURL)

	// Create run
	resp, err := client.CreateRun(submission)
	if err != nil {
		telemetry.RecordError(Version, telemetry.ClassifyError(err))
		return fmt.Errorf("failed to create run: %w", err)
	}

	fmt.Printf("Run created: %s\n", resp.ID)

	// Set up context with Ctrl+C handler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		fmt.Fprintf(os.Stderr, "\nInterrupted, stopping run...\n")
		if err := client.StopRun(resp.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to stop run: %v\n", err)
		}
		cancel()
		// Second signal: force quit immediately
		<-sigCh
		fmt.Fprintf(os.Stderr, "\nForce quit.\n")
		os.Exit(1)
	}()

	// Stream results
	result, err := client.StreamRun(ctx, resp.ID, os.Stdout)
	if err != nil {
		telemetry.RecordError(Version, telemetry.ClassifyError(err))
		if ctx.Err() != nil {
			return fmt.Errorf("run cancelled")
		}
		return fmt.Errorf("stream error: %w", err)
	}

	// Record telemetry
	total := result.Passed + result.Failed + result.Skipped
	telemetry.RecordRun(telemetry.RunParams{
		Version:       Version,
		DurationMs:    time.Since(startTime).Milliseconds(),
		CloudMode:     true,
		HasTagsFilter: len(filterTags) > 0,
		HasNameFilter: filterName != "",
	}, total, result.Passed, result.Failed, result.Skipped, len(tests.Suites))

	// Print link
	fmt.Printf("\n  %s/runs/%s\n", strings.TrimSuffix(apiURL, "/"), resp.ID)

	if result.HasFailures() {
		return exitErrorf(ExitTestFailure, "test run failed: %d test(s) failed", result.Failed)
	}

	return nil
}
