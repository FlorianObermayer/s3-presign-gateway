BIN := s3-presign-gateway
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: run test build docker clean

run:
	go run .

test:
	go vet ./...
	go test -race ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(BIN) .

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(BIN):$(VERSION) .

clean:
	rm -rf bin
