package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/kjbreil/opti/internal/config"
	"github.com/kjbreil/opti/internal/database"
	"github.com/kjbreil/opti/internal/logger"
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
	logLevel       = flag.String("log-level", "", "Logging level: debug, info, warn, error")
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

	// Initialize logger
	log := initializeLogger(cfg)

	db := initializeDatabase(cfg, log)
	queueProc := queue.NewQueueProcessor(db, cfg, log)
	webServer := createWebServer(db, cfg, queueProc, log)

	runServicesUntilShutdown(cfg, webServer, queueProc, db, log)
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
		LogLevel:     *logLevel,
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

// initializeLogger creates and configures the logger based on config settings.
func initializeLogger(cfg config.Config) *slog.Logger {
	// Determine log level
	var level slog.Level
	if cfg.Silent {
		// Use a level higher than Error to effectively disable logging
		level = logger.LevelDisabled
	} else if cfg.LogLevel != "" {
		var err error
		level, err = logger.ParseLevel(cfg.LogLevel)
		if err != nil {
			// Log the error to stderr and use default level
			fmt.Fprintf(os.Stderr, "opti: invalid log level %q: %v, using default\n", cfg.LogLevel, err)
			level = logger.DefaultLevel
		}
	} else {
		level = logger.DefaultLevel
	}

	// TEMPORARY: Force debug logging for troubleshooting
	level = slog.LevelDebug

	return logger.NewLogger(level, os.Stdout)
}

// initializeDatabase initializes the database and returns the connection.
func initializeDatabase(cfg config.Config, log *slog.Logger) *gorm.DB {
	var err error
	db, err := database.InitDB(cfg.WorkDir, cfg.Debug, log)
	if err != nil {
		log.Error("Failed to initialize database", slog.Any("error", err))
		os.Exit(1)
	}

	log.Info("Database initialized")

	return db
}

// createWebServer creates and initializes the web server.
func createWebServer(db *gorm.DB, cfg config.Config, queueProc *queue.Processor, log *slog.Logger) *web.Server {
	var err error
	webServer, err := web.NewServer(db, cfg, queueProc, log)
	if err != nil {
		log.Error("Failed to create web server", slog.Any("error", err))
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
	log *slog.Logger,
) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	var wg sync.WaitGroup

	startWebServer(ctx, &wg, webServer, log)
	startQueueProcessor(ctx, &wg, queueProc, log)
	startScanner(ctx, &wg, cfg, log)

	<-sigChan
	log.Info("Shutting down")
	cancel()

	wg.Wait()
	log.Info("Shutdown complete")
}

// startWebServer starts the web server in a goroutine.
func startWebServer(ctx context.Context, wg *sync.WaitGroup, webServer *web.Server, log *slog.Logger) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		var startErr error
		if startErr = webServer.Start(ctx, *port); startErr != nil {
			log.Error("Web server error", slog.Any("error", startErr))
		}
	}()
}

// startQueueProcessor starts the queue processor in a goroutine.
func startQueueProcessor(ctx context.Context, wg *sync.WaitGroup, queueProc *queue.Processor, log *slog.Logger) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		var procErr error
		if procErr = queueProc.Start(ctx); procErr != nil {
			log.Error("Queue processor error", slog.Any("error", procErr))
		}
	}()
}

// startScanner starts the scanner in a goroutine.
func startScanner(ctx context.Context, wg *sync.WaitGroup, cfg config.Config, log *slog.Logger) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		var runErr error
		if runErr = scanner.Run(ctx, cfg, log); runErr != nil && !errors.Is(runErr, context.Canceled) {
			log.Error("Scanner error", slog.Any("error", runErr))
		}
	}()
}
