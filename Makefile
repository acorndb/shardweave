GO ?= go
# Keep in sync with .github/workflows/ci.yml golangci-lint-action version.
GOLANGCI_VERSION ?= v2.15.0

.PHONY: build test test-unit test-integration lint fmt tidy

build:
	$(GO) build ./...

test: test-unit test-integration

# -short 跳过需要 Docker 的差分校准
test-unit:
	$(GO) test -race -short -count=1 ./...

test-integration:
	$(GO) test -race -count=1 -timeout 20m -v ./test/differential/ -run 'TestNullProxyCalibration|TestPassthroughDifferential|TestPassthroughQueryScenarios'
	$(GO) test -race -count=1 -timeout 20m -v ./test/handshake/ -run TestClientDial_RealMySQL
	$(GO) test -race -count=1 -timeout 20m -v ./test/passthrough/ -run TestPassthrough_Select1
	$(GO) test -race -count=1 -timeout 15m -v ./test/rwsplit/ -run TestRWSplitRealTopology
	$(GO) test -race -count=1 -timeout 15m -v ./test/sharding/ -run TestSingleShardRouting

# golangci-lint must be built with a Go version >= the go directive in
# go.mod; prebuilt binaries and `go run` toolchain switching both fail that
# check, so force the current toolchain when building from source.
lint:
	$(GO) vet ./...
	GOTOOLCHAIN=$$($(GO) env GOVERSION) $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run

fmt:
	go run mvdan.cc/gofumpt@latest -extra -w . 2>/dev/null || gofmt -w .

tidy:
	$(GO) mod tidy
