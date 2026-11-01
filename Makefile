GO ?= go
# Keep in sync with .github/workflows/ci.yml golangci-lint-action version.
GOLANGCI_VERSION ?= v2.15.0
# Topology for the fixed Docker fixture: single | rwsplit | sharding.
MODE ?= single
# Optional subtest filter, e.g. CASE=sql_type_ (appended to TestSQLCorpus/).
CASE ?=

.PHONY: build test test-unit coverage test-integration lint fmt tidy e2e-up e2e-down e2e-logs e2e-test e2e-mtr-record e2e-corpus bin/shardweave

build:
	$(GO) build ./...

test: test-unit test-integration

# -short 跳过需要 Docker 的差分校准
test-unit:
	$(GO) test -race -short -count=1 ./...

# 单元测试 + 覆盖率（CI 上传 Codecov；coverage.out 已被 .gitignore 忽略）
coverage:
	$(GO) test -race -short -count=1 -covermode=atomic -coverprofile=coverage.out ./...

test-integration:
	$(GO) test -race -count=1 -timeout 20m -v ./test/differential/ -run 'TestNullProxyCalibration|TestPassthroughDifferential|TestPassthroughQueryScenarios'
	$(GO) test -race -count=1 -timeout 20m -v ./test/handshake/ -run TestClientDial_RealMySQL
	$(GO) test -race -count=1 -timeout 20m -v ./test/passthrough/ -run TestPassthrough_Select1
	$(GO) test -race -count=1 -timeout 15m -v ./test/rwsplit/ -run TestRWSplitRealTopology
	$(GO) test -race -count=1 -timeout 15m -v ./test/sharding/ -run TestSingleShardRouting
	$(GO) test -race -count=1 -timeout 20m -v ./test/gorm/ -run 'TestGORM_'

# ---- Fixed Docker fixture (docker/README.md) ----
# Cross-compile for the Linux container; the image is scratch + this binary.
bin/shardweave:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -o bin/shardweave ./cmd/shardweave

e2e-up: bin/shardweave
	@case "$(MODE)" in single|rwsplit|sharding) ;; *) echo "MODE must be single|rwsplit|sharding" >&2; exit 1;; esac
	docker compose -f docker/compose-$(MODE).yaml up -d --build

e2e-down:
	@case "$(MODE)" in single|rwsplit|sharding) ;; *) echo "MODE must be single|rwsplit|sharding" >&2; exit 1;; esac
	docker compose -f docker/compose-$(MODE).yaml down -v

e2e-logs:
	docker compose -f docker/compose-$(MODE).yaml logs -f shardweave

# MTR golden tests: normal runs only touch the proxy; record goldens once from
# the direct/single endpoint with e2e-mtr-record (records both suites).
e2e-test:
	@case "$(MODE)" in single|sharding) ;; *) echo "MODE must be single|sharding" >&2; exit 1;; esac
	@name=TestMTR; [ "$(MODE)" = "sharding" ] && name=TestMTRSharding; \
	if [ -n "$(CASE)" ]; then filter="^$${name}$$/$(CASE)"; else filter="^$${name}$$"; fi; \
	PROXY_DSN="app:app-pass-123@tcp(127.0.0.1:3307)/bench?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s" \
	$(GO) test -count=1 -v ./test/mtr/ -run "$$filter"

e2e-mtr-record:
	@case "$(MODE)" in single) ;; *) echo "record from single: make e2e-up MODE=single then e2e-mtr-record" >&2; exit 1;; esac
	DIRECT_DSN="app:app-pass-123@tcp(127.0.0.1:3308)/bench?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s" \
	SINGLE_DSN="app:app-pass-123@tcp(127.0.0.1:3308)/bench?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s" \
	$(GO) test -count=1 -v ./test/mtr/ -run 'TestMTR' -update

# Optional authoring aid: live direct-vs-proxy differential over the YAML corpus.
e2e-corpus:
	@case "$(MODE)" in single) ;; *) echo "SQL corpus currently supports MODE=single" >&2; exit 1;; esac
	DIRECT_DSN="app:app-pass-123@tcp(127.0.0.1:3308)/bench?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s" \
	PROXY_DSN="app:app-pass-123@tcp(127.0.0.1:3307)/bench?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s" \
	$(GO) test -count=1 -v ./test/corpus/ -run "TestSQLCorpus$(if $(CASE),/$(CASE),)"

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
