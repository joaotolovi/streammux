.PHONY: help deps build frontend backend run dev clean test vet fmt

BINARY := bin/streammux
GO ?= go

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-12s\033[0m %s\n", $$1, $$2}'

deps: ## Install Go deps and frontend deps
	$(GO) mod download
	cd web && npm install

frontend: ## Build the React frontend (required before go build)
	cd web && npm run build

backend: ## Build the Go binary (frontend must be built first)
	$(GO) build -o $(BINARY) ./cmd/streammux

build: frontend backend ## Build frontend + backend

run: ## Build and run (set env vars first, see README)
	$(MAKE) build
	./$(BINARY)

dev: ## Run the Go server with local frontend dev server proxying
	@echo "In one terminal: cd web && npm run dev"
	@echo "In another: PORT=3001 $(GO) run ./cmd/streammux"

test: ## Run Go tests
	$(GO) test ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format Go code
	gofmt -w .

clean: ## Remove build artifacts
	rm -rf $(BINARY) web/dist
