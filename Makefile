BINARY := enclava
GOFMT_FILES := $(shell rg --files -g '*.go' -g '!.git/**')
LINT := golangci-lint

.PHONY: build run test fmt fmt-check vet lint ci entrypoint-smoke

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(BINARY) ./cmd/enclava

run:
	go run ./cmd/enclava

test:
	go test ./...
	./scripts/entrypoint_smoke.sh

fmt:
	gofmt -w $(GOFMT_FILES)

fmt-check:
	@if [ -n "$$(gofmt -l $(GOFMT_FILES))" ]; then \
		echo "gofmt required for:"; \
		gofmt -l $(GOFMT_FILES); \
		exit 1; \
	fi

vet:
	go vet ./...

lint:
	@if ! command -v $(LINT) >/dev/null; then \
		echo "missing $(LINT). install with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		exit 1; \
	fi
	$(LINT) run ./...

ci: fmt-check vet test lint

entrypoint-smoke:
	./scripts/entrypoint_smoke.sh
