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
	"github.com/kjbreil/opti/internal/runner"
	"github.com/kjbreil/opti/internal/web"
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

var (
	sourceDirs   stringSlice
	workDir      = flag.String("w", "", "Working directory (temp/state & outputs)")
	interactive  = flag.Bool("I", false, "Interactive (prompt) mode")
	keep         = flag.Bool("k", false, "Keep intermediates (reserved)")
	silent       = flag.Bool("S", false, "Silent (less console output)")
	workers      = flag.Int("j", 1, "Parallel workers")
	scanInterval = flag.Int("scan-interval", 5, "Minutes between scans (0 = scan once and exit)")
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
	port           = flag.Int("port", 8181, "HTTP server port for web interface")
)

const Version = "0.3.3"

func main() {
	flag.Var(&sourceDirs, "s", "Source directory of videos (can be specified multiple times)")
	flag.Parse()
	if *version {
		fmt.Println("opti", Version)
		return
	}

	// Handle --generate-config flag
	if *generateConfig != "" {
		if err := config.GenerateDefault(*generateConfig); err != nil {
			fmt.Fprintf(os.Stderr, "opti: failed to generate config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Generated default config file at: %s\n", *generateConfig)
		return
	}

	if *listHW {
		if err := runner.PrintHardwareCaps(*ffmpegPath); err != nil {
			fmt.Fprintln(os.Stderr, "opti:", err)
			os.Exit(1)
		}
		return
	}

	// Load config from file if specified
	// Support both -c and --config flags (--config takes precedence if both are set)
	configFilePath := *configPath
	if *configPathLong != "" {
		configFilePath = *configPathLong
	}

	var loadedConfig *runner.Config
	if configFilePath != "" {
		var err error
		loadedConfig, err = config.LoadFromFile(configFilePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "opti: failed to load config file: %v\n", err)
			os.Exit(1)
		}
	}

	// Build CLI flags config
	cliConfig := runner.Config{
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

	// Merge configs if a config file was loaded
	// CLI flags take precedence over config file values
	cfg := cliConfig
	if loadedConfig != nil {
		cfg = *config.MergeConfigs(loadedConfig, &cliConfig)
	}

	// Validate merged configuration
	if err := runner.ValidateEngine(cfg.Engine, cfg.FFmpegPath); err != nil {
		fmt.Fprintln(os.Stderr, "opti:", err)
		os.Exit(1)
	}
	if len(cfg.SourceDirs) == 0 || cfg.WorkDir == "" {
		fmt.Fprintln(os.Stderr, "usage: -s <source> -w <workdir> [options]")
		os.Exit(2)
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}

	// Normalize config paths (set defaults for ffmpeg/ffprobe)
	runner.NormalizeConfig(&cfg)

	// Initialize database
	db, err := runner.InitDB(cfg.WorkDir, cfg.Debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opti: failed to initialize database: %v\n", err)
		os.Exit(1)
	}
	if !cfg.Silent {
		fmt.Println("Database initialized.")
	}

	// Create queue processor
	queueProc := runner.NewQueueProcessor(db, cfg)

	// Create web server
	webServer, err := web.NewServer(db, cfg, queueProc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opti: failed to create web server: %v\n", err)
		os.Exit(1)
	}

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Wait group for all goroutines
	var wg sync.WaitGroup

	// Start web server
	wg.Add(1)
	go func() {
		defer wg.Done()
		var startErr error
		if startErr = webServer.Start(ctx, *port); startErr != nil {
			fmt.Fprintf(os.Stderr, "opti: web server error: %v\n", startErr)
		}
	}()

	// Start queue processor
	wg.Add(1)
	go func() {
		defer wg.Done()
		var procErr error
		if procErr = queueProc.Start(ctx); procErr != nil {
			fmt.Fprintf(os.Stderr, "opti: queue processor error: %v\n", procErr)
		}
	}()

	// Start scanner
	wg.Add(1)
	go func() {
		defer wg.Done()
		var runErr error
		if runErr = runner.Run(ctx, cfg); runErr != nil && !errors.Is(runErr, context.Canceled) {
			fmt.Fprintf(os.Stderr, "opti: scanner error: %v\n", runErr)
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	if !cfg.Silent {
		fmt.Println("\nShutting down...")
	}
	cancel()

	// Wait for all goroutines to finish
	wg.Wait()
	if !cfg.Silent {
		fmt.Println("Shutdown complete.")
	}
}
