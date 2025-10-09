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
	sourceDir   = flag.String("s", "", "Source directory of videos")
	workDir     = flag.String("w", "", "Working directory (temp/state & outputs)")
	interactive = flag.Bool("I", false, "Interactive (prompt) mode")
	keep        = flag.Bool("k", false, "Keep intermediates (reserved)")
	silent      = flag.Bool("S", false, "Silent (less console output)")
	workers     = flag.Int("j", 1, "Parallel workers")
	engine      = flag.String("engine", "cpu", "Engine: cpu|qsv")
	ffmpegPath  = flag.String("ffmpeg", "ffmpeg", "Path to ffmpeg binary")
	listHW      = flag.Bool("list-hw", false, "Detect and print available hardware accelerators/encoders, then exit")
	version     = flag.Bool("version", false, "Print version and exit")
)

const Version = "0.2.1"

func main() {
	swapInplace := flag.Bool("swap-inplace", false, "After a successful transcode, rename the source to <name.ext>.original and copy the new file back to the original path (same name/ext).")
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
		SourceDir:   *sourceDir,
		WorkDir:     *workDir,
		Interactive: *interactive,
		Keep:        *keep,
		Silent:      *silent,
		Workers:     *workers,
		Engine:      *engine,
		FFmpegPath:  *ffmpegPath,
		SwapInplace: *swapInplace,
	}
	if err := runner.Run(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "opti:", err)
		os.Exit(1)
	}
}
