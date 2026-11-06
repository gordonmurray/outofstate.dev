# Every target runs inside Docker. No Go toolchain is needed on the host.
# Tool versions live in tools/Dockerfile and nowhere else.

UID := $(shell id -u)
GID := $(shell id -g)
TOOLS_IMAGE := oos-tools:local
CACHE_VOLUME := oos-cache
MODULE := github.com/gordonmurray/outofstate.dev

# Attach a TTY only when there is one, so the same targets work in CI.
TTY := $(shell [ -t 0 ] && echo -it)

RUN := docker run --rm $(TTY) \
	--user $(UID):$(GID) \
	-v "$(CURDIR)":/src \
	-v $(CACHE_VOLUME):/cache \
	-e GITHUB_TOKEN \
	$(TOOLS_IMAGE)

.DEFAULT_GOAL := help
.PHONY: help tools volumes test lint vuln build snapshot release release-check tidy check-versions ci shell clean

help: ## Show targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-15s %s\n", $$1, $$2}'

tools: ## Build the pinned tools image from tools/Dockerfile
	@docker build -q --target tools -t $(TOOLS_IMAGE) -f tools/Dockerfile tools >/dev/null

# Docker creates named volumes owned by root. Hand the cache to the caller.
volumes: tools
	@docker volume create $(CACHE_VOLUME) >/dev/null
	@docker run --rm --user 0 -v $(CACHE_VOLUME):/cache $(TOOLS_IMAGE) sh -c \
		'[ "$$(stat -c %u:%g /cache)" = "$(UID):$(GID)" ] || chown -R $(UID):$(GID) /cache'

test: volumes ## Run tests with the race detector
	$(RUN) go test -race ./...

lint: volumes ## go vet and golangci-lint
	$(RUN) sh -c 'go vet ./... && golangci-lint run ./...'

vuln: volumes ## Check dependencies for known vulnerabilities
	$(RUN) go tool govulncheck ./...

build: volumes ## Build a static linux binary into dist/oos
	$(RUN) sh -c 'CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X $(MODULE)/internal/version.Version=dev-$$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" \
		-o dist/oos ./cmd/oos'

snapshot: volumes ## Local GoReleaser build without publishing
	$(RUN) goreleaser release --snapshot --clean

release-check: volumes ## Validate the GoReleaser config, then build a snapshot
	$(RUN) goreleaser check
	$(MAKE) snapshot

release: volumes ## Publish a release for the current tag (CI only, needs GITHUB_TOKEN)
	$(RUN) goreleaser release --clean

tidy: volumes ## go mod tidy
	$(RUN) go mod tidy

check-versions: volumes ## Go in the tools image must match go.mod and tools/Dockerfile
	@want=$$(awk '/^toolchain /{print $$2}' go.mod); \
	have=$$($(RUN) go env GOVERSION | tr -d '\r'); \
	pin=go$$(sed -n 's/^FROM golang:\([0-9.]*\)-.*/\1/p' tools/Dockerfile); \
	echo "go.mod toolchain: $$want | tools/Dockerfile: $$pin | tools image: $$have"; \
	[ "$$want" = "$$have" ] && [ "$$have" = "$$pin" ] || { echo "Go versions disagree" >&2; exit 1; }

ci: check-versions lint test vuln build release-check ## Everything CI runs

shell: volumes ## Interactive shell in the tools image
	docker run --rm -it --user $(UID):$(GID) -v "$(CURDIR)":/src -v $(CACHE_VOLUME):/cache $(TOOLS_IMAGE) bash

clean: ## Remove build output
	rm -rf ./dist
