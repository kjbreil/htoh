# Technical Patterns and Implementation Details

## FFmpeg Integration Patterns

### Progress Parsing
FFmpeg is invoked with `-progress pipe:1` which outputs key=value pairs to stdout:
- `out_time_ms`: Current position in microseconds
- `fps`: Current frames per second
- `speed`: Encoding speed multiplier (e.g., "2.5x")
- `total_size`: Output file size in bytes
- `progress=end`: Encoding complete signal

### Encoder-Specific Arguments

**CPU (libx265):**
```
-c:v libx265 -preset medium -crf <value>
```

**Intel Quick Sync (QSV):**
```
-hwaccel qsv -c:v h264_qsv (input)
-c:v hevc_qsv -preset veryfast -global_quality <ICQ>
```

**NVIDIA NVENC:**
```
-c:v hevc_nvenc -preset medium -rc:v vbr -cq <value> -b:v 0
```

Common across all: `-c:a copy -c:s copy` (preserve audio/subtitle streams)

## File Path Handling
- Source files maintain relative path structure in work directory
- Output naming: `<base>.hevc.mkv` or `<base>.hevc.mp4`
- State file: `.hevc_state.tsv` in work directory
- Original backup: `<name.ext>.original` when using swap-inplace

## Error Handling Patterns
- Non-fatal errors (like ffprobe failures) mark files as "failed" in state and continue
- Fatal errors (like state file corruption) return early
- Worker errors are collected in buffered error channel
- Final summary reports error count

## Goroutine Patterns
1. **Worker Pool:** Fixed number of workers consuming from job channel
2. **Progress Renderer:** Single goroutine updating terminal UI
3. **FFmpeg Progress Parser:** One per worker, reads stdout pipe
4. **Wait Coordination:** WaitGroup + done channel pattern

## State Persistence Strategy
- Append-only TSV format for simplicity and crash-safety
- In-memory map synchronized with file
- Status transitions: (none) → queued → processing → done/failed
- Resume logic: Skip files already marked "done" or "processing"

## Quality Calculation Logic
Quality is based on bits-per-pixel (BPP):
```
BPP = bitrate / (width × height × fps)
```

BPP thresholds (for base quality before clamping):
- ≥0.18 → 25
- ≥0.14 → 26
- ≥0.11 → 27
- ≥0.09 → 28
- ≥0.075 → 29
- ≥0.06 → 30
- ≥0.045 → 31
- >0 → 32

Fallback to bitrate-only heuristics if dimensions/fps unavailable.

Engine-specific adjustments:
- CPU: CRF = clamp(base, 16, 35)
- QSV: ICQ = clamp(base-1, 1, 51)
- NVENC: CQ = clamp(base-1, 0, 51)

Fast mode: Increment base by 1 (lower quality = smaller file)

## Container Selection
- Default: MKV for universal compatibility
- MP4 with faststart: For streaming (web playback)
- Flag behavior:
  - `-output-mp4`: Force all outputs to MP4
  - `-faststart-mp4`: Keep MP4 sources as MP4, others as MKV

## Swap-Inplace Strategy
1. Check source and output exist
2. Generate backup path: `<original>.original`
3. Rename source to backup
4. Attempt rename (move) of output to source location
5. If rename fails (cross-filesystem), copy instead
6. Preserve file extensions exactly

## Debug Logging
When `-debug` flag is set:
- File discovery: Print each scanned/probed file
- FFprobe path resolution
- Quality calculation details (bitrate, BPP, CRF/ICQ/CQ values)
- Queue operations
