# Common tasks. Run `make help` to list them.
#
# The database: if you have Docker, `make db` starts Postgres in a container.
# If not, `make devdb` runs the same Postgres as a plain process (no Docker
# needed). Either way it listens on localhost:5432 as juju/juju.

.PHONY: help db devdb dev test test-integration lint fmt fuzz docs sdk sdk-test example-web example-web-build example-rn-deps flutter-test dashboard dashboard-dev dashboard-test

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

example-web-build: ## Minify the web example's app.js with a source map, like a production build
	sdks/node_modules/.bin/esbuild examples/web/app.js --bundle --minify --sourcemap --target=es2020 --outfile=examples/web/dist/app.min.js

example-web: ## Serve the web example at http://127.0.0.1:5173/examples/web/ (run `make sdk` and `make example-web-build` first)
	python3 -m http.server 5173 --bind 127.0.0.1

example-rn-deps: ## Build and pack the SDKs, then install them into examples/react-native
	cd sdks && npm install && npm run build
	mkdir -p examples/react-native/vendor
	cd sdks && npm pack -w @juju/core -w @juju/react-native --pack-destination ../examples/react-native/vendor
	# Name the tarballs: a plain `npm install` keeps the old SDK when the
	# version number hasn't changed, and the app would run stale code.
	cd examples/react-native && npm install ./vendor/juju-core-*.tgz ./vendor/juju-react-native-*.tgz

flutter-test: ## Format check, analyze and test the Flutter SDK
	cd sdks/flutter && flutter pub get && dart format --set-exit-if-changed lib test && flutter analyze && flutter test

dashboard: ## Build the dashboard and embed it in the server (then `make dev` serves it at :8080)
	cd dashboard && npm ci && npm run build
	find server/internal/web/dist -mindepth 1 ! -name placeholder.html -delete
	cp -R dashboard/dist/. server/internal/web/dist/

dashboard-dev: ## Dashboard with hot reload at http://localhost:5174 (needs `make dev` running)
	cd dashboard && npm run dev

dashboard-test: ## Type-check and unit test the dashboard
	cd dashboard && npm run typecheck && npm test
