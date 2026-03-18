#!/usr/bin/env bash
set -euo pipefail

# Run all tests (unit + integration/envtest) for the controller package.
# Prerequisites: Go 1.24+

GOBIN="$(go env GOPATH)/bin"
export PATH="${GOBIN}:${PATH}"

echo "==> Installing setup-envtest..."
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

echo "==> Downloading envtest binaries (k8s 1.31)..."
KUBEBUILDER_ASSETS=$("${GOBIN}/setup-envtest" use 1.31 -p path)
export KUBEBUILDER_ASSETS
echo "    KUBEBUILDER_ASSETS=${KUBEBUILDER_ASSETS}"

echo "==> Running go mod tidy..."
go mod tidy

echo "==> Running tests..."
go test ./internal/controller/... -v -count=1 -timeout 120s
