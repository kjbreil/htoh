package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/kjbreil/opti/internal/config"
	"github.com/kjbreil/opti/internal/database"
	"github.com/kjbreil/opti/internal/queue"
	"github.com/kjbreil/opti/internal/runner"
	"github.com/kjbreil/opti/internal/scanner"
	"github.com/kjbreil/opti/internal/web"
	"gorm.io/gorm"
)

// stringSlice is a custom flag type that allows multiple values.
type stringSlice []string

func (s *stringSlice) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

//nolint:gochecknoglobals // CLI flag variables must be global for flag package
var (
	sourceDirs   stringSlice
	workDir      = flag.String("w", "", "Working directory (temp/state & outputs)")
	interactive  = flag.Bool("I", false, "Interactive (prompt) mode")
	keep         = flag.Bool("k", false, "Keep intermediates (reserved)")
	silent       = flag.Bool("S", false, "Silent (less console output)")
	workers      = flag.Int("j", 1, "Parallel workers")
	scanInterval = flag.Int("scan-interval", defaultScanInterval, "Minutes between scans (0 = scan once and exit)")
	engine       = flag.String("engine", "cpu", "Engine: cpu|qsv|nvenc|vaapi|videotoolbox")
	device       = flag.String("device", "", "Hardware device path for VAAPI (default: /dev/dri/renderD128)")
	ffmpegPath   = flag.String("ffmpeg", "ffmpeg", "Path to ffmpeg binary")
	ffprobePath  = flag.String(
		"ffprobe",
		"",
		"Path to ffprobe binary (defaults to sibling of -ffmpeg or system ffprobe)",
	)
	debugLogging = flag.Bool("debug", false, "Enable verbose logging (file discovery, ffprobe calls)")
	forceMP4     = flag.Bool("output-mp4", false, "Force outputs to MP4 container with -movflags +faststart")
	faststartMP4 = flag.Bool(
		"faststart-mp4",
		false,
		"Keep MP4 container when source is MP4 and add -movflags +faststart",
	)
	fastMode = flag.Bool(
		"fast",
		false,
		"Favor smaller files by lowering quality targets one notch across all engines",
	)
	listHW         = flag.Bool("list-hw", false, "Detect and print available hardware accelerators/encoders, then exit")
	version        = flag.Bool("version", false, "Print version and exit")
	configPath     = flag.String("c", "", "Path to TOML config file (config values override CLI flags)")
	configPathLong = flag.String("config", "", "Path to TOML config file (same as -c)")
	generateConfig = flag.String("generate-config", "", "Generate a default config file at the specified path and exit")
	port           = flag.Int("port", defaultWebPort, "HTTP server port for web interface")
)

const (
	Version = "0.3.3"

	// Default configuration values.
	defaultScanInterval = 5    // minutes between scans
	defaultWebPort      = 8181 // HTTP server port

	// Exit codes.
	exitCodeUsageError = 2
)

func main() {
	flag.Var(&sourceDirs, "s", "Source directory of videos (can be specified multiple times)")
	flag.Parse()

	if handleEarlyExitFlags() {
		return
	}

	cfg := loadAndMergeConfig()
	validateAndNormalizeConfig(&cfg)

	db := initializeDatabase(cfg)
	queueProc := queue.NewQueueProcessor(db, cfg)
	webServer := createWebServer(db, cfg, queueProc)

	runServicesUntilShutdown(cfg, webServer, queueProc, db)
}

// handleEarlyExitFlags handles flags that cause early exit (version, generate-config, list-hw).
// Returns true if the program should exit.
func handleEarlyExitFlags() bool {
	if *version {
		fmt.Fprintf(os.Stdout, "opti %s\n", Version)
		return true
	}

	if *generateConfig != "" {
		if err := config.GenerateDefault(*generateConfig); err != nil {
			fmt.Fprintf(os.Stderr, "opti: failed to generate config: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Generated default config file at: %s\n", *generateConfig)
		return true
	}

	if *listHW {
		if err := runner.PrintHardwareCaps(*ffmpegPath); err != nil {
			fmt.Fprintln(os.Stderr, "opti:", err)
			os.Exit(1)
		}
		return true
	}

	return false
}

// loadAndMergeConfig loads configuration from file (if specified) and merges with CLI flags.
func loadAndMergeConfig() config.Config {
	configFilePath := getConfigFilePath()
	loadedConfig := loadConfigFile(configFilePath)
	cliConfig := buildCLIConfig()

	if loadedConfig != nil {
		return *config.MergeConfigs(loadedConfig, &cliConfig)
	}
	return cliConfig
}

// getConfigFilePath returns the config file path, preferring --config over -c.
func getConfigFilePath() string {
	if *configPathLong != "" {
		return *configPathLong
	}
	return *configPath
}

// loadConfigFile loads config from file if path is provided.
func loadConfigFile(path string) *config.Config {
	if path == "" {
		return nil
	}

	cfg, err := config.LoadFromFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opti: failed to load config file: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

// buildCLIConfig constructs config from CLI flags.
func buildCLIConfig() config.Config {
	return config.Config{
		SourceDirs:   sourceDirs,
		WorkDir:      *workDir,
		Interactive:  *interactive,
		Keep:         *keep,
		Silent:       *silent,
		Workers:      *workers,
		ScanInterval: *scanInterval,
		Engine:       *engine,
		VAAPIDevice:  *device,
		FFmpegPath:   *ffmpegPath,
		FFprobePath:  *ffprobePath,
		Debug:        *debugLogging,
		ForceMP4:     *forceMP4,
		FaststartMP4: *faststartMP4,
		FastMode:     *fastMode,
	}
}

// validateAndNormalizeConfig validates and normalizes the configuration.
func validateAndNormalizeConfig(cfg *config.Config) {
	if err := runner.ValidateEngine(cfg.Engine, cfg.FFmpegPath); err != nil {
		fmt.Fprintln(os.Stderr, "opti:", err)
		os.Exit(1)
	}

	if len(cfg.SourceDirs) == 0 || cfg.WorkDir == "" {
		fmt.Fprintln(os.Stderr, "usage: -s <source> -w <workdir> [options]")
		os.Exit(exitCodeUsageError)
	}

	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}

	config.NormalizeConfig(cfg)
}

// initializeDatabase initializes the database and returns the connection.
func initializeDatabase(cfg config.Config) *gorm.DB {
	db, err := database.InitDB(cfg.WorkDir, cfg.Debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opti: failed to initialize database: %v\n", err)
		os.Exit(1)
	}

	if !cfg.Silent {
		fmt.Fprintf(os.Stdout, "Database initialized.\n")
	}

	return db
}

// createWebServer creates and initializes the web server.
func createWebServer(db *gorm.DB, cfg config.Config, queueProc *queue.Processor) *web.Server {
	webServer, err := web.NewServer(db, cfg, queueProc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opti: failed to create web server: %v\n", err)
		os.Exit(1)
	}
	return webServer
}

// runServicesUntilShutdown starts all services and waits for shutdown signal.
func runServicesUntilShutdown(
	cfg config.Config,
	webServer *web.Server,
	queueProc *queue.Processor,
	_ *gorm.DB,
) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	var wg sync.WaitGroup

	startWebServer(ctx, &wg, webServer)
	startQueueProcessor(ctx, &wg, queueProc)
	startScanner(ctx, &wg, cfg)

	<-sigChan
	if !cfg.Silent {
		fmt.Fprintf(os.Stdout, "\nShutting down...\n")
	}
	cancel()

	wg.Wait()
	if !cfg.Silent {
		fmt.Fprintf(os.Stdout, "Shutdown complete.\n")
	}
}

// startWebServer starts the web server in a goroutine.
func startWebServer(ctx context.Context, wg *sync.WaitGroup, webServer *web.Server) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		var startErr error
		if startErr = webServer.Start(ctx, *port); startErr != nil {
			fmt.Fprintf(os.Stderr, "opti: web server error: %v\n", startErr)
		}
	}()
}

// startQueueProcessor starts the queue processor in a goroutine.
func startQueueProcessor(ctx context.Context, wg *sync.WaitGroup, queueProc *queue.Processor) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		var procErr error
		if procErr = queueProc.Start(ctx); procErr != nil {
			fmt.Fprintf(os.Stderr, "opti: queue processor error: %v\n", procErr)
		}
	}()
}

// startScanner starts the scanner in a goroutine.
func startScanner(ctx context.Context, wg *sync.WaitGroup, cfg config.Config) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		var runErr error
		if runErr = scanner.Run(ctx, cfg); runErr != nil && !errors.Is(runErr, context.Canceled) {
			fmt.Fprintf(os.Stderr, "opti: scanner error: %v\n", runErr)
		}
	}()
}
