# Project Overview

## Purpose
`opti` is a command-line tool for batch transcoding H.264/AVC videos to H.265/HEVC format using FFmpeg. It provides:
- Automatic video discovery and codec detection with ffprobe
- Parallel transcoding with configurable worker count
- Hardware acceleration support (CPU libx265, Intel Quick Sync, NVIDIA NVENC)
- Live progress dashboard
- Persistent state tracking for resume capability
- Optional in-place file swapping with backup

## Current Version
v0.3.3

## Tech Stack
- **Language:** Go 1.24.2 (minimum 1.21+)
- **External Tools:** FFmpeg and FFprobe (required on PATH or via flags)
- **Dependencies:** Pure Go standard library (no external Go modules)
- **Module Path:** github.com/kjbreil/opti

## Repository Structure
```
/home/kjell/htoh/
├── cmd/opti/           # CLI entry point
│   ├── main.go         # Flag parsing and main function
│   └── swap_inplace.go # In-place swap logic
├── internal/runner/    # Core business logic
│   ├── runner.go       # Main orchestration, worker pool, transcoding
│   ├── state.go        # TSV-based state persistence
│   ├── ffprobe.go      # Video probing and codec detection
│   ├── ffmpeg_caps.go  # Hardware capability detection
│   ├── progress.go     # Live terminal dashboard
│   └── swap_inplace.go # File swapping utilities
├── bin/                # Compiled binary output
├── Makefile            # Build targets
├── go.mod              # Go module definition
└── README.md           # Comprehensive documentation
```

## Key Features
1. **Adaptive Quality:** Automatically calculates CRF/ICQ/CQ values based on source bitrate, resolution, and FPS
2. **Resume Support:** State file (.hevc_state.tsv) tracks queued/processing/done/failed status
3. **Container Options:** MKV by default, MP4 with faststart for streaming
4. **Hardware Engines:** cpu (libx265), qsv (Intel Quick Sync), nvenc (NVIDIA)
5. **Swap Inplace:** Replaces original with transcoded file, keeping .original backup
