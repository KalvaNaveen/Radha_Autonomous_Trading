.PHONY: deps build test vet check backtest dist
deps:
	go mod tidy
build:
	go build -trimpath -o bin/radha-engine ./cmd/engine
	go build -trimpath -o bin/radha-backtest ./cmd/backtest
	go build -trimpath -o bin/kitemock ./tools/kitemock
test:
	go test -race -count=1 ./...
vet:
	go vet ./...
check: build
	./bin/radha-engine -check
backtest: build
	./bin/radha-backtest -years 5
dist:
	for t in windows/amd64 darwin/arm64 darwin/amd64 linux/amd64; do \
	  os=$${t%/*}; ar=$${t#*/}; ext=""; [ $$os = windows ] && ext=".exe"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$ar go build -trimpath -ldflags "-s -w" -o dist/radha-engine-$$os-$$ar$$ext ./cmd/engine; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$ar go build -trimpath -ldflags "-s -w" -o dist/radha-backtest-$$os-$$ar$$ext ./cmd/backtest; \
	done
