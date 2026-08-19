BINARY  ?= mqttview
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help build build-web build-go run dev test lint fmt clean docker

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: build-web build-go ## Build the frontend and the binary

build-web: ## Build the frontend into web/dist
	cd web && npm ci && npm run build

build-go: ## Build the binary, embedding whatever is in web/dist
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/mqttview

run: build ## Build and run on :8114
	./$(BINARY) -addr 127.0.0.1:8114

dev: ## Run the API only; use `cd web && npm run dev` alongside it
	go run ./cmd/mqttview -addr 127.0.0.1:8114 -log-level debug

test: ## Run the Go tests against an in-process broker
	go test ./...

lint: ## Vet the Go code and type-check the frontend
	go vet ./...
	cd web && npx tsc -b --noEmit

fmt: ## Format the Go code
	gofmt -w ./cmd ./internal

docker: ## Build the container image
	docker build -t $(BINARY):$(VERSION) .

clean: ## Remove build output
	rm -f $(BINARY)
	rm -rf web/dist/assets
