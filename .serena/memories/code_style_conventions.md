# Code Style and Conventions

## Formatting
- **Standard Go formatting:** Use `gofmt` or `goimports`
- **Indentation:** Tabs (standard Go convention)
- **Line length:** No strict limit, but keep reasonable

## Naming Conventions
- **Exported symbols:** CamelCase (Config, Run, PrintHardwareCaps, ProbeInfo)
- **Unexported symbols:** camelCase (job, candidate, qualityChoice, transcode)
- **Package names:** Short, lowercase, single word (runner)
- **File names:** lowercase with underscores (swap_inplace.go, ffmpeg_caps.go)

## Code Organization
- **Package structure:** 
  - `cmd/` for entry points
  - `internal/` for private packages
- **File permissions:** Octal notation (0o755, 0o644)
- **Error handling:** 
  - Use `fmt.Errorf` for error creation
  - Error wrapping with `%w` for context
  - Check errors inline with `if err != nil`
- **Struct definitions:** Define types for configuration and data structures
- **Concurrency:**
  - Use `context.Context` for cancellation
  - `sync.Mutex` / `sync.RWMutex` for state protection
  - `sync.WaitGroup` for goroutine coordination
  - Channels for worker communication

## Documentation
- Minimal inline comments observed in the codebase
- Public APIs should have doc comments (though not heavily documented currently)
- README.md is the primary documentation source

## Best Practices
- Use standard library exclusively where possible
- Prefer explicit error returns over panics
- Use defer for cleanup (Close, Unlock, Cancel)
- Keep functions focused and reasonably sized
- Use struct literals with field names for clarity
