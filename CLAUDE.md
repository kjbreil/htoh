# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`opti` is a Go-based H.264 to HEVC video transcoder (v0.3.3) that scans directories, transcodes videos using ffmpeg with hardware acceleration support, and maintains persistent state for resumable operations. It includes a web interface for queue management and real-time progress monitoring.

## Build and Development Commands

### Building
```bash
make build              # Format, vet, and build with version info
go build -o ./bin/opti ./cmd/opti  # Simple build without version
```

The build outputs to `./bin/opti`. Version metadata is injected via git tags during `make build`.

### Testing
```bash
make test               # Run all tests
go test ./...           # Alternative test command
```

### Running
```bash
make run ARGS="-s /path/to/source -w /path/to/work"  # Run with args
./bin/opti -s <source> -w <work> [options]           # Direct execution
```

### Linting
The project uses `golangci-lint` with a strict configuration (`.golangci.yml`). Key linters enabled:
- Standard Go tools: `govet`, `staticcheck`, `errcheck`, `ineffassign`
- Security: `gosec` - never use `-no-verify` flags with git commands
- Code quality: `gocyclo`, `funlen`, `cyclop`, `revive`
- Formatters: `goimports`, `golines` (max line length: 120)

Auto-fix enabled for most issues. Shadow variable declarations are checked with strict mode.

### Cleaning
```bash
make clean              # Remove ./bin directory
```

## Architecture

### Core Components

1. **Main Entry Point** (`cmd/opti/main.go`)
   - Handles CLI flag parsing and TOML config file loading
   - Orchestrates three concurrent subsystems with graceful shutdown:
     - Scanner (`runner.Run`) - discovers and catalogs video files
     - Queue Processor (`runner.QueueProcessor`) - processes transcoding jobs
     - Web Server (`web.Server`) - serves UI and API endpoints
   - Uses `context.Context` for cancellation propagation

2. **Runner Package** (`internal/runner/`)
   - **Core Modules**:
     - `runner.go` - File discovery scanner that walks source directories
     - `queue.go` - Worker pool that processes transcoding jobs from database queue
     - `ffprobe.go` - Video file probing using ffprobe (extracts codec, resolution, bitrate, etc.)
     - `ffmpeg_caps.go` - Hardware capability detection (lists available encoders)
     - `database.go` - GORM-based SQLite persistence for files, media info, queue items, and task logs
     - `progress.go` - Thread-safe progress tracking for live dashboard
     - `swap_inplace.go` - Atomic file replacement after successful transcode

   - **Transcoding Flow**:
     1. Scanner discovers H.264 files and stores metadata in database
     2. Files are queued for processing (manual via web UI or automatic)
     3. Workers pick up queued items and call `transcode()` function
     4. `transcode()` builds ffmpeg command based on engine (cpu/qsv/nvenc/vaapi/videotoolbox)
     5. Progress is streamed from ffmpeg's `-progress pipe:1` output
     6. On success, original file is atomically replaced via `SwapInPlaceCopy()`

3. **Web Package** (`internal/web/`)
   - `server.go` - HTTP server with SSE (Server-Sent Events) for real-time updates
   - `events.go` - Event broadcasting system for log streaming and progress updates
   - Templates in `internal/web/templates/` - HTML UI for file browsing and queue management
   - **Key Endpoints**:
     - `/api/tree` - Returns directory structure with file metadata
     - `/api/convert` - Adds files/folders to queue
     - `/api/events` - SSE stream for real-time log and progress updates
     - `/api/queue` - Queue status and management

4. **Config Package** (`internal/config/`)
   - `config.go` - TOML configuration file parsing and merging with CLI flags
   - Config file takes precedence over CLI flags when specified with `-c`

### Database Schema

SQLite database (`.opti.db` in work directory) with tables:
- `files` - Discovered video files (path, size, modtime, status)
- `media_info` - Detailed codec information (resolution, bitrate, color space, etc.)
- `queue_items` - Transcoding queue (status: queued/processing/done/failed)
- `task_logs` - Detailed logs for each queue item with timestamps

### Hardware Acceleration

Supports five encoding engines:
- `cpu` (libx265) - Software encoding, most compatible
- `qsv` (Intel Quick Sync) - Intel hardware encoding
- `nvenc` (NVIDIA) - NVIDIA GPU encoding
- `vaapi` (Video Acceleration API) - Intel/AMD GPUs on Linux (auto-detects driver: iHD for Intel, radeonsi for AMD)
- `videotoolbox` - Apple hardware encoding (macOS)

Engine validation happens at startup via `ValidateEngine()`. VAAPI device paths are validated and GPU vendor is auto-detected from sysfs.

### Quality Heuristics

The `deriveQualityChoice()` function calculates adaptive quality targets:
- Inspects source bitrate and bits-per-pixel (bpp) to determine appropriate CRF/ICQ/CQ values
- Aims for ~50% file size reduction while preserving visual quality
- `-fast` flag shifts quality one notch toward smaller files
- Different scales for different engines: CRF (16-35), ICQ (1-51), CQ (0-51)

### State Management

- **Scanner**: Continuously scans at configured intervals (`-scan-interval`, 0 = scan once and exit)
- **Database**: Tracks file discovery state to avoid re-probing unchanged files
- **Queue**: Workers poll database every 2 seconds for new items
- **Interruption**: Safe to Ctrl+C - in-flight transcodes complete current tick, state is persisted

## Important Patterns

### Error Handling
- Always initialize error variables before use to avoid shadow declarations
- When multiple variables need initialization, use `var err error` first, then `=` assignment (not `:=`)
- Example from user's global instructions:
  ```go
  var err error
  otherVar, err = someFunc()  // Use = not := to avoid shadowing err
  ```

### Concurrency
- All background goroutines receive `context.Context` for cancellation
- Use `sync.WaitGroup` to ensure clean shutdown
- Queue processor uses worker pool pattern with buffered job channel

### File Operations
- Use `SwapInPlaceCopy()` for atomic file replacement (creates `.original` backup, then renames)
- All temp files are created in work directory root (flat structure)
- Work directory contains: `.opti.db`, `.hevc_state.tsv` (deprecated), and output files

### Configuration Precedence
When `-c` config file is specified:
1. Config file values are loaded
2. CLI flags override config values if explicitly set (non-default)
3. Special handling for booleans, slices, and zero-value defaults

### Progress Broadcasting
- `Prog` type provides thread-safe progress updates via `Update()` method
- Progress is parsed from ffmpeg's key=value output on stdout
- SSE broadcasts progress every 500ms to connected web clients

## Code Quality Standards

- **No global loggers**: Use `fmt.Fprintf(os.Stderr, ...)` or pass logger instances
- **Context awareness**: All long-running operations must accept `context.Context`
- **Struct field initialization**: Initialize all fields explicitly (exhaustruct linter)
- **Error wrapping**: Wrap external package errors with context using `fmt.Errorf(..., %w, err)`
- **gosec compliance**: Use `#nosec` comments with justification only when necessary
- **Line length**: 120 characters max (enforced by golines)
- **Function complexity**: Max 30 cyclomatic complexity, 20 cognitive complexity
- **Function length**: Max 100 lines, 50 statements

## Testing Notes

- Test files exempt from some linters: `bodyclose`, `dupl`, `errcheck`, `funlen`, `goconst`, `gosec`, `noctx`, `wrapcheck`
- Place test files in same package as code under test
- Use table-driven tests for multiple test cases

## Special Considerations

- **ffmpeg/ffprobe paths**: Auto-detected as siblings if one is specified
- **Container format**: MKV by default, MP4 if `-output-mp4` or source is MP4 with `-faststart-mp4`
- **Multiple source directories**: Supported via repeated `-s` flags or config file `source_dirs` array
- **Web server**: Runs on port 8181 by default (configurable with `-port`)
- **Scan interval**: Set to 0 for single-scan mode (useful for batch processing)

## Dependencies

- `gorm.io/gorm` - ORM for database operations
- `github.com/glebarez/sqlite` - Pure Go SQLite driver
- `github.com/BurntSushi/toml` - TOML config parsing
- Go 1.24.2+ required (uses range over integer feature)

Technical approach:

- Uses semantic overlap to override CC's default parallel execution behavior
- Explicit precedence declarations with concrete harm justification
- Based on LLM instruction conflict research (2024-2025)