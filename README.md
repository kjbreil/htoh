# opti

`htoh` scans a directory of H.264 / AVC videos, transcodes them to H.265 / HEVC with `ffmpeg`, and keeps track of progress so long-running batches can resume safely. It ships with a live terminal dashboard, parallel workers, and optional in-place swapping once a transcode finishes.

## Features
- Automatic file discovery with `ffprobe`, limited to H.264 footage.
- Parallel `ffmpeg` transcodes (`-j` workers) with CPU (`libx265`) or Intel Quick Sync (`-engine qsv`).
- Live progress table that refreshes a few times a second.
- Persistent state file (`.hevc_state.tsv` in the work directory) to allow resuming after interruptions.
- Optional interactive confirmation and a silent mode for unattended runs.
- `--swap-inplace` mode that safely replaces the source with the transcoded output while keeping a `.original` backup.

## Requirements
- Go 1.21 or newer to build the CLI.
- `ffmpeg` and `ffprobe` available on the PATH (or provide the full path with `-ffmpeg`).  
  `ffprobe` is used for codec detection, so both tools must come from the same build.  
- For `-engine qsv`, the host must have Intel Quick Sync Video support and the appropriate drivers.

## Installation
Clone the project and build the binary:

```bash
git clone <repo-url> opti
cd opti
GOCACHE=$(pwd)/.gocache go build ./...
# or: make build
```

The compiled binary will be at `./cmd/opti/opti` when using `go build`, or `./bin/opti` when using the provided `Makefile`.

You can also install straight into your `GOBIN` for everyday use:

```bash
GO111MODULE=on go install ./cmd/opti
```

(Add `GOCACHE=$(pwd)/.gocache` in front if your environment cannot write to the default Go build cache.)

## Command Overview

`opti` expects a **source** directory containing the original videos and a **work** directory where it can store intermediate files, outputs, and state.

```
opti -s <source-dir> -w <work-dir> [options]
```

### Flag reference

| Flag | Description | Default |
| --- | --- | --- |
| `-s` | Source directory that will be scanned for candidate videos. | *(required)* |
| `-w` | Working/output directory used for encoded files and `.hevc_state.tsv`. | *(required)* |
| `-j` | Number of parallel workers. `opti` also caps to at least 1. | `1` |
| `-engine` | Transcode engine: `cpu` (libx265) or `qsv` (Intel Quick Sync). | `cpu` |
| `-ffmpeg` | Path to the `ffmpeg` binary. A sibling `ffprobe` will be used if found. | `ffmpeg` |
| `-I` | Interactive mode—asks for confirmation before processing batch. | `false` |
| `-S` | Silent mode—suppresses the live dashboard and most logs. | `false` |
| `-k` | Reserved flag for keeping intermediates. Currently ignored. | `false` |
| `--swap-inplace` | After a successful encode, rename the source to `<name>.original` and move the output back to the original location. | `false` |
| `-version` | Print version information and exit. | `false` |

### What happens during a run
1. The source directory is walked and video files (`.mkv`, `.mp4`, `.mov`, `.m4v`) are probed with `ffprobe`. Only H.264/AVC videos are queued.  
2. Discovered files are recorded in `work/.hevc_state.tsv` with their status (`queued`, `processing`, `done`, `failed`). This allows `opti` to resume automatically if you restart it later.  
3. Each worker calls `ffmpeg` with `-progress pipe:1` and feeds progress updates into the dashboard.  
4. Finished encodes are written under the work directory, preserving the relative path and appending `.hevc.mkv`.  
5. With `--swap-inplace`, the tool renames the source file to `<name>.original` and moves the encoded file back to `<name>` (falling back to a copy if the directories are on different filesystems).  

Interrupting the program (Ctrl+C) allows in-flight workers to finish their current tick. Re-running the command will skip everything marked `done` in the state file.

## Usage Examples

### Basic CPU transcode
```bash
opti -s /mnt/media/source -w /mnt/media/opti-work
```

### Run four parallel workers and keep the UI quiet
```bash
opti -s ~/Videos/raw -w ~/Videos/opti-work -j 4 -S
```

### Confirm before running and replace sources in place
```bash
opti -s /srv/nas/series -w /srv/nas/opti-temp -I --swap-inplace
```

### Use Intel Quick Sync with a custom ffmpeg build
```bash
opti -s /mnt/videos -w /mnt/opti/work -j 3 \
     -engine qsv -ffmpeg /opt/ffmpeg-qsv/bin/ffmpeg
```

## Development

The repository includes helper targets:

```bash
make build   # gofmt, govet, go build -trimpath (output in ./bin/opti)
make test    # go test ./...
make clean   # remove ./bin
```

When contributing, keep the live dashboard responsive and document new flags or engines in this README so end users know how to take advantage of them.
