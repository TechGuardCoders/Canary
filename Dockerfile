# Multi-stage, multi-arch (linux/amd64, linux/arm64) — base images are official
# multi-arch builds, so `docker buildx build --platform linux/amd64,linux/arm64` works as-is.
#
# NOTE (Go variant): Canary also ships a Go implementation (canary.go).
# To build/test that instead, swap the builder stage for:
#   FROM golang:1.22-slim AS go-builder
#   WORKDIR /app
#   COPY go.mod canary.go canary_test.go ./
#   RUN go test ./...
# The runtime stage below stays unchanged.

FROM python:3.12-slim AS builder
WORKDIR /app
COPY canary.py pytest.ini ./
COPY tests ./tests
RUN pip install --no-cache-dir pytest

FROM python:3.12-slim
WORKDIR /app
COPY --from=builder /usr/local/lib/python3.12/site-packages /usr/local/lib/python3.12/site-packages
COPY --from=builder /app /app
# Library repo: default command runs the test suite as a smoke check.
CMD ["python", "-m", "pytest", "-q"]
