.PHONY: build test test-e2e fixtures lint tidy

# Build the CLI into bin/.
build: tidy
	go build -o bin/createapt-go ./cmd/createapt-go

# Fast unit + golden tests (no servers, no containers).
test: tidy
	go tool gotestsum --format testname ./...

# End-to-end tests: drive the built binary against local disk, a fresh MinIO
# instance (S3), a local sshd (SFTP), a fake-gcs-server container (GCS), and a
# read-only HTTP server, then install from the result with a real apt.
# Backends whose dependencies are absent skip themselves.
# See test/e2e/README.md.
test-e2e: tidy
	go tool gotestsum --format testname -- -tags e2e ./test/e2e/... -timeout 600s

# Regenerate the sample .deb and source-package fixtures (needs dpkg-deb).
fixtures:
	./reference/gen.sh

lint:
	golangci-lint run

tidy:
	@go mod tidy
