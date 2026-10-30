GO ?= go
GOLANGCI ?= $(shell command -v golangci-lint 2>/dev/null || echo "$(HOME)/go/bin/golangci-lint")

.PHONY: build test test-unit test-integration lint fmt tidy

build:
	$(GO) build ./...

test: test-unit test-integration

# -short 跳过需要 Docker 的差分校准
test-unit:
	$(GO) test -race -short -count=1 ./...

test-integration:
	$(GO) test -race -count=1 -timeout 20m -v ./test/differential/ -run 'TestNullProxyCalibration|TestPassthroughDifferential'
	$(GO) test -race -count=1 -timeout 20m -v ./test/handshake/ -run TestClientDial_RealMySQL
	$(GO) test -race -count=1 -timeout 20m -v ./test/passthrough/ -run TestPassthrough_Select1

lint:
	$(GO) vet ./...
	$(GOLANGCI) run

fmt:
	go run mvdan.cc/gofumpt@latest -extra -w . 2>/dev/null || gofmt -w .

tidy:
	$(GO) mod tidy
