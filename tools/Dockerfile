# Single source of truth for every pinned tool version.
# Dependabot (docker ecosystem, directory /tools) updates the FROM lines.
# The Makefile builds the "tools" stage as the local image oos-tools:local.

FROM golangci/golangci-lint:v2.15.0 AS golangci-lint
FROM goreleaser/goreleaser:v2.20.0 AS goreleaser

# Go 1.27.3 is the only Go toolchain used for building, testing and linting.
# The tool binaries are copied in so that they all run against this Go.
FROM golang:1.27.3-trixie AS tools
COPY --from=golangci-lint /usr/bin/golangci-lint /usr/local/bin/golangci-lint
COPY --from=goreleaser /usr/bin/goreleaser /usr/local/bin/goreleaser
ENV GOTOOLCHAIN=local \
    GOMODCACHE=/cache/gomod \
    GOCACHE=/cache/gobuild \
    GOLANGCI_LINT_CACHE=/cache/golangci-lint \
    HOME=/tmp
# The repo is mounted from the host. In CI the mounting user can differ from
# the owner, which git would otherwise refuse.
RUN git config --system --add safe.directory /src
WORKDIR /src

# Used only by "make e2e" (M3): Terraform plus the AWS CLI.
FROM hashicorp/terraform:1.17.1 AS terraform
