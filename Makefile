# Common tasks. Run `make help` to list them.
#
# The database: if you have Docker, `make db` starts Postgres in a container.
# If not, `make devdb` runs the same Postgres as a plain process (no Docker
# needed). Either way it listens on localhost:5432 as juju/juju.

.PHONY: help db devdb dev test test-integration lint fmt fuzz docs sdk sdk-test example-web example-rn-deps

help:
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

db: ## Start Postgres with docker compose
	docker compose up -d postgres

devdb: ## Start Postgres without Docker (keep this terminal open)
	cd server && go run ./cmd/devdb -dir ../.devdb

dev: ## Run the juju server (needs `make db` or `make devdb` first)
	cd server && JUJU_LOG_LEVEL=debug go run ./cmd/juju serve

test: lint ## Format check, vet, and unit tests with the race detector
	cd server && go test -race -count=1 ./...

test-integration: ## Tests against a real Postgres (starts an embedded one)
	cd server && go test -race -count=1 -tags=integration ./...

lint: ## gofmt + go vet (+ golangci-lint if installed)
	@cd server && test -z "$$(gofmt -l .)" || (echo "not gofmt'd:"; gofmt -l .; exit 1)
	cd server && go vet ./... && go vet -tags=integration ./...
	@command -v golangci-lint >/dev/null && (cd server && golangci-lint run) || true

fmt: ## Format all Go code
	cd server && gofmt -w .

fuzz: ## Fuzz the event parser for 30s
	cd server && go test ./internal/event -run='^$$' -fuzz=FuzzNormalize -fuzztime=30s

docs: ## Rebuild the GitHub Pages files in docs/ from docs/_src/
	./scripts/build-docs.sh

sdk: ## Install, type-check, test and build the JavaScript SDKs
	cd sdks && npm install && npm run check

sdk-test: ## Run the JavaScript SDK tests
	cd sdks && npm test

example-web: ## Serve the web example at http://localhost:5173/examples/web/ (run `make sdk` first)
	python3 -m http.server 5173 --bind 127.0.0.1

example-rn-deps: ## Build and pack the SDKs, then install them into examples/react-native
	cd sdks && npm install && npm run build
	mkdir -p examples/react-native/vendor
	cd sdks && npm pack -w @juju/core -w @juju/react-native --pack-destination ../examples/react-native/vendor
	cd examples/react-native && npm install
