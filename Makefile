BINARY=opti

.PHONY: all build clean test lint run
all: build

build:
	go fmt ./...
	go vet ./...
	go build -trimpath -ldflags="-s -w -X main.Version=$$(git describe --tags --always 2>/dev/null || echo dev)" -o bin/$(BINARY) ./cmd/opti

run: build
	./bin/$(BINARY) $(ARGS)

test:
	go test ./...

clean:
	rm -rf bin
