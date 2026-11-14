BIN := s3-presign-gateway
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: run test build docker cors clean

run:
	go run .

test:
	go vet ./...
	go test -race ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(BIN) .

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(BIN):$(VERSION) .

# Writes the bucket CORS rule using the S3_ADMIN_* credentials from .env.
# Override the action with: make cors ACTION=show
ACTION ?= apply
cors:
	docker compose run --rm -e ACTION=$(ACTION) cors-setup

clean:
	rm -rf bin
