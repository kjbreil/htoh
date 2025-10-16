# Architecture Notes

## High-Level Flow
1. **CLI Entry (cmd/opti/main.go)**: Parse flags, validate inputs, create Config
2. **Runner Orchestration (internal/runner/runner.go)**: Main Run() function coordinates the entire process
3. **File Discovery**: Walk source directory, probe videos with ffprobe
4. **State Management**: Load/save TSV state file for resume capability
5. **Worker Pool**: Spawn N goroutines that consume jobs from a channel
6. **Transcoding**: Each worker calls ffmpeg with appropriate encoder and quality settings
7. **Progress Tracking**: Live dashboard updates via goroutine-safe Progress object
8. **Optional Swap**: Replace original with transcoded file if --swap-inplace is set

## Key Components

### State Management (state.go)
- TSV file format: `<filepath>\t<status>\n`
- Thread-safe with RWMutex
- Status values: queued, processing, done, failed
- Append-only writes for simplicity

### Quality Selection (runner.go)
- Calculates bits-per-pixel (BPP) from source bitrate, resolution, and FPS
- Maps BPP ranges to quality values:
  - CPU: CRF (16-35, default 22)
  - QSV: ICQ (1-51)
  - NVENC: CQ (0-51)
- Fast mode shifts quality one step toward smaller files

### Progress Dashboard (progress.go)
- Live terminal UI using ANSI escape codes
- Updates multiple times per second
- Per-worker rows showing file, phase, FPS, speed, time, size
- Thread-safe updates via mutex

### FFprobe Integration (ffprobe.go)
- Executes ffprobe with JSON output
- Parses stream info: codec, width, height, fps, bitrate
- Filters H.264/AVC videos only

### Hardware Capabilities (ffmpeg_caps.go)
- Queries ffmpeg for available hardware accelerators
- Validates requested encoder exists
- Used by -list-hw flag

### Swap Inplace (swap_inplace.go)
- Renames source to .original
- Moves/copies transcoded file to original location
- Falls back to copy if cross-filesystem

## Concurrency Model
- Channel-based worker pool
- Each worker processes jobs until channel closes
- WaitGroup tracks worker completion
- Context for cancellation support (not fully utilized yet)
- Progress updates via goroutine-safe Update() method

## Extension Points
- New encoders: Add case to transcode() switch statement
- Quality heuristics: Modify estimateQualityLevel() and deriveQualityChoice()
- Container formats: Extend container selection logic in Run()
- State formats: Replace State implementation
