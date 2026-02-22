BINARY          := bin/sharemk
BINARY_ARM64    := bin/sharemk-linux-arm64
BINARY_AMD64    := bin/sharemk-linux-amd64
CMD             := ./cmd/sharemk
IMAGE           := sharemk:latest
SERVER          := root@share.mk
VERSION         ?= dev

export GOPATH := $(HOME)/go

.PHONY: build build-arm64 build-amd64 build-all run test tidy docker-build docker-run deploy setup-server

## Local development
build:
	go build -o $(BINARY) $(CMD)

run: build
	@set -a && source .env && set +a && ./$(BINARY)

test:
	go test ./...

tidy:
	go mod tidy

## Docker
docker-build:
	docker build -t $(IMAGE) .

docker-run:
	docker run --rm --env-file .env -p 8080:8080 $(IMAGE)

## Production builds
build-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o $(BINARY_ARM64) $(CMD)

build-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o $(BINARY_AMD64) $(CMD)

build-all: build-arm64 build-amd64

# First-time server setup (run once)
setup-server:
	ssh $(SERVER) "bash -s" < deploy/setup.sh
	@echo ""
	@echo "Now copy your .env to the server:"
	@echo "  scp .env $(SERVER):/opt/sharemk/.env"

# Deploy arm64 binary and restart the service
deploy: build-arm64
	scp $(BINARY_ARM64) $(SERVER):/opt/sharemk/sharemk.new
	ssh $(SERVER) "mv /opt/sharemk/sharemk.new /opt/sharemk/sharemk && systemctl restart sharemk && systemctl status sharemk --no-pager -l"
