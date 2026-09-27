.PHONY: deps build test check vet
deps:
	go mod tidy
build:
	go build -trimpath -ldflags "-s -w -X main.version=$$(git describe --tags --always 2>/dev/null || echo dev)" -o bin/engine ./cmd/engine
test:
	go test -race -count=1 ./...
vet:
	go vet ./...
check: build
	./bin/engine -config config.yaml -check
