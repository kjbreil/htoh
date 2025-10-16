# Task Completion Checklist

When completing a development task on this project, follow these steps:

## 1. Format Code
```bash
go fmt ./...
```
All Go files should be formatted using `gofmt` before committing.

## 2. Run Vet
```bash
go vet ./...
```
Check for common Go programming errors.

## 3. Run Tests
```bash
go test ./...
```
Ensure all existing tests pass. Add new tests if adding significant functionality.

## 4. Build
```bash
make build
# or
go build -o ./bin/opti ./cmd/opti
```
Verify the code compiles successfully.

## 5. Test Manually (if applicable)
Run the binary with test data to verify functionality:
```bash
./bin/opti -s <test-source> -w <test-work> [options]
```

## 6. Update Documentation
- Update README.md if adding new features or changing flags
- Update flag reference table if adding new command-line options
- Add usage examples for new functionality

## 7. Version Control
```bash
git status
git add <files>
git commit -m "descriptive commit message"
```

## Notes
- The Makefile `build` target runs `go fmt`, `go vet`, and `go build` in sequence
- There is no CI/CD pipeline configured currently
- Tests are minimal in the current codebase
- The project uses semantic versioning (currently v0.3.3)
- Version metadata is injected via ldflags during build using git describe
