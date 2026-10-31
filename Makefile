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

lint:
	$(GO) vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	elif [ -x "$(HOME)/go/bin/golangci-lint" ]; then \
		"$(HOME)/go/bin/golangci-lint" run; \
	else \
		$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run; \
	fi

fmt:
	go run mvdan.cc/gofumpt@latest -extra -w . 2>/dev/null || gofmt -w .

tidy:
	$(GO) mod tidy
