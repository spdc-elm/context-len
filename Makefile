.PHONY: bootstrap test test-go test-frontend build-frontend run frontend-dev start-local

bootstrap:
	cd frontend && npm ci --no-audit --no-fund

test: test-go test-frontend build-frontend

test-go:
	go test -race ./...
	go vet ./...

test-frontend:
	cd frontend && npm test -- --run

build-frontend:
	cd frontend && npm run build

run:
	go run ./cmd/context-lens

start-local:
	./scripts/start-local.sh
