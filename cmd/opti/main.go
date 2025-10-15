package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"opti.local/opti/internal/runner"
)

var (
	sourceDir    = flag.String("s", "", "Source directory of videos")
	workDir      = flag.String("w", "", "Working directory (temp/state & outputs)")
	interactive  = flag.Bool("I", false, "Interactive (prompt) mode")
	keep         = flag.Bool("k", false, "Keep intermediates (reserved)")
	silent       = flag.Bool("S", false, "Silent (less console output)")
	workers      = flag.Int("j", 1, "Parallel workers")
	engine       = flag.String("engine", "cpu", "Engine: cpu|qsv|nvenc|vaapi")
	device       = flag.String("device", "", "Hardware device path for VAAPI (default: /dev/dri/renderD128)")
	ffmpegPath   = flag.String("ffmpeg", "ffmpeg", "Path to ffmpeg binary")
	ffprobePath  = flag.String("ffprobe", "", "Path to ffprobe binary (defaults to sibling of -ffmpeg or system ffprobe)")
	debugLogging = flag.Bool("debug", false, "Enable verbose logging (file discovery, ffprobe calls)")
	forceMP4     = flag.Bool("output-mp4", false, "Force outputs to MP4 container with -movflags +faststart")
	faststartMP4 = flag.Bool("faststart-mp4", false, "Keep MP4 container when source is MP4 and add -movflags +faststart")
	fastMode     = flag.Bool("fast", false, "Favor smaller files by lowering quality targets one notch across all engines")
	listHW       = flag.Bool("list-hw", false, "Detect and print available hardware accelerators/encoders, then exit")
	version      = flag.Bool("version", false, "Print version and exit")
)

const Version = "0.3.3"

func main() {
	swapInplace := flag.Bool("swap-inplace", false, "After a successful transcode, rename the source to <name.ext>.original and copy the new file back to the original path (same name/ext).")
	deleteOriginal := flag.Bool("delete-original", false, "Delete the .original backup file after a successful swap (requires --swap-inplace).")
	flag.Parse()
	if *version {
		fmt.Println("opti", Version)
		return
	}
	if *listHW {
		if err := runner.PrintHardwareCaps(*ffmpegPath); err != nil {
			fmt.Fprintln(os.Stderr, "opti:", err)
			os.Exit(1)
		}
		return
	}
	if *deleteOriginal && !*swapInplace {
		fmt.Fprintln(os.Stderr, "opti: --delete-original requires --swap-inplace")
		os.Exit(2)
	}
	if err := runner.ValidateEngine(*engine, *ffmpegPath); err != nil {
		fmt.Fprintln(os.Stderr, "opti:", err)
		os.Exit(1)
	}
	if *sourceDir == "" || *workDir == "" {
		fmt.Fprintln(os.Stderr, "usage: -s <source> -w <workdir> [options]")
		os.Exit(2)
	}
	if *workers <= 0 {
		*workers = 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// In case someone wants a hard cap:
	_ = ctx
	_ = time.Hour

	cfg := runner.Config{
		SourceDir:      *sourceDir,
		WorkDir:        *workDir,
		Interactive:    *interactive,
		Keep:           *keep,
		Silent:         *silent,
		Workers:        *workers,
		Engine:         *engine,
		VAAPIDevice:    *device,
		FFmpegPath:     *ffmpegPath,
		FFprobePath:    *ffprobePath,
		Debug:          *debugLogging,
		ForceMP4:       *forceMP4,
		FaststartMP4:   *faststartMP4,
		FastMode:       *fastMode,
		SwapInplace:    *swapInplace,
		DeleteOriginal: *deleteOriginal,
	}
	if err := runner.Run(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "opti:", err)
		os.Exit(1)
	}
}
