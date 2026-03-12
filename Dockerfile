# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /workspace

# Copy go mod files and download dependencies first (better layer caching).
COPY go.mod go.sum ./
RUN go mod download

# Copy source code.
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build the binary with static linking for distroless compatibility.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o manager \
    ./cmd/

# Final image: distroless for minimal attack surface.
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
