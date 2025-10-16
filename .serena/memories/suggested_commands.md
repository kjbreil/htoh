# Suggested Commands

## Build Commands
```bash
# Build the binary (runs gofmt, govet, and build with version injection)
make build

# Manual build without Makefile
go build -trimpath -ldflags="-s -w -X main.Version=$(git describe --tags --always 2>/dev/null || echo dev)" -o bin/opti ./cmd/opti

# Simple build without version metadata
go build -o ./bin/opti ./cmd/opti

# Build with custom GOCACHE (if needed)
GOCACHE=$(pwd)/.gocache go build -o ./bin/opti ./cmd/opti
```

## Testing Commands
```bash
# Run all tests
make test

# Run tests manually
go test ./...
```

## Formatting and Linting
```bash
# Format all Go files
go fmt ./...

# Vet code for issues
go vet ./...
```

## Clean Up
```bash
# Remove built binaries
make clean

# Manual cleanup
rm -rf bin
```

## Running the Application
```bash
# Basic usage (after building)
./bin/opti -s <source-dir> -w <work-dir>

# With build and run
make run ARGS="-s /path/to/source -w /path/to/work"

# Common development usage
./bin/opti -s ~/test-videos -w ~/opti-work -j 2 -debug

# List hardware capabilities
./bin/opti -list-hw

# With hardware acceleration
./bin/opti -s ~/videos -w ~/work -engine qsv
./bin/opti -s ~/videos -w ~/work -engine nvenc

# Print version
./bin/opti -version
```

## System Commands (Linux)
Standard Linux commands are available:
- `ls`, `cd`, `pwd` - file navigation
- `grep`, `find` - searching
- `git` - version control
- `ffmpeg`, `ffprobe` - video processing (required dependencies)
