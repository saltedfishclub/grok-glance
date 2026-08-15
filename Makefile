# grok-glance
#
# Two build systems, one binary: the frontend is compiled by Vite into web/dist
# and then embedded by `go build`. That ordering is not optional — `//go:embed`
# resolves at compile time, so a Go build always ships whatever `make web` last
# produced.

GO      ?= go
NPM     ?= npm
ADDR    ?= :7717
BIN     := bin/glance
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Vite's dev server. Its proxy sends /api to the Go server, which is what keeps
# the `__Host-` session cookie working in development: the cookie is
# SameSite=Strict and would never be sent cross-origin.
WEB_PORT ?= 5173

.PHONY: all
all: build

# ── build ────────────────────────────────────────────────────────────────────

.PHONY: build
build: web
	@mkdir -p bin
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/glance
	@echo "built $(BIN) ($(VERSION))"

## Go binary without rebuilding the frontend — fast loop for server work.
.PHONY: server
server:
	@mkdir -p bin
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/glance

.PHONY: web
web: web/node_modules
	cd web && $(NPM) run build
# Vite empties dist/ on every build, taking the .gitkeep with it. The file is
# what lets `go build` succeed on a clean clone that has never run npm, so it
# has to come back or the next contributor gets an unexplainable embed error.
	@touch web/dist/.gitkeep

web/node_modules: web/package.json
	cd web && $(NPM) install
	@touch web/node_modules

# ── development ──────────────────────────────────────────────────────────────

## Go server + Vite with hot reload. Open http://localhost:$(WEB_PORT).
.PHONY: dev
dev: server web/node_modules
	@echo "glance on $(ADDR), UI on http://localhost:$(WEB_PORT)"
	@trap 'kill 0' EXIT INT TERM; \
	  $(BIN) serve --addr $(ADDR) & \
	  cd web && $(NPM) run dev -- --port $(WEB_PORT); \
	  wait

## Run the built binary against the embedded UI (production shape).
.PHONY: run
run: build
	$(BIN) serve --addr $(ADDR)

## A fake grok on the agent socket -- UI work without rebuilding Rust.
## KEY comes from `glance apikey add dev`, or $$GLANCE_API_KEY.
KEY ?= $(GLANCE_API_KEY)
.PHONY: fakeagent
fakeagent:
	$(GO) run ./cmd/fakeagent --key "$(KEY)"

# ── checks ───────────────────────────────────────────────────────────────────

.PHONY: test
test:
	$(GO) test ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: typecheck
typecheck: web/node_modules
	cd web && $(NPM) run typecheck

.PHONY: fmt
fmt:
	$(GO) fmt ./...

## Everything CI would run.
.PHONY: check
check: vet test typecheck

# ── housekeeping ─────────────────────────────────────────────────────────────

.PHONY: clean
clean:
	rm -rf bin web/dist
	@mkdir -p web/dist && touch web/dist/.gitkeep

.PHONY: distclean
distclean: clean
	rm -rf web/node_modules

.PHONY: help
help:
	@echo "grok-glance targets:"
	@echo "  make build      frontend + binary -> $(BIN)"
	@echo "  make dev        Go server + Vite dev server (hot reload)"
	@echo "  make run        build, then serve on $(ADDR)"
	@echo "  make fakeagent  connect a fake grok (KEY=glance_sk_...)"
	@echo "  make web        frontend only"
	@echo "  make server     binary only (keeps the last frontend build)"
	@echo "  make check      vet + go test + tsc"
	@echo "  make clean      drop bin/ and web/dist"
