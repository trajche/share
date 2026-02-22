BINARY      := bin/sharemk
BINARY_LINUX := bin/sharemk-linux
CMD         := ./cmd/sharemk
IMAGE       := sharemk:latest
SERVER      := root@share.mk

export GOPATH := $(HOME)/go

.PHONY: build build-linux run test tidy docker-build docker-run deploy setup-server

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

## Production
build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BINARY_LINUX) $(CMD)

# First-time server setup (run once)
setup-server:
	ssh $(SERVER) "bash -s" < deploy/setup.sh
	@echo ""
	@echo "Now copy your .env to the server:"
	@echo "  scp .env $(SERVER):/opt/sharemk/.env"

# Deploy a new binary and restart the service
deploy: build-linux
	scp $(BINARY_LINUX) $(SERVER):/opt/sharemk/sharemk
	ssh $(SERVER) "systemctl restart sharemk && systemctl status sharemk --no-pager -l"
