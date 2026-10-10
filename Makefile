.PHONY: up down dev-native test test-go test-web e2e e2e-native fmt build

up:
	bash scripts/compose.sh up --build -d --scale worker=2

down:
	bash scripts/compose.sh down

dev-native:
	bash scripts/dev-native.sh

test:
	bash scripts/compose.sh --profile test run --build --rm test

test-go:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'Set TEST_DATABASE_URL to run real PostgreSQL tests.'; exit 1)
	go test -race -count=1 -v ./...

test-web:
	cd web && npm run typecheck && npm run build

e2e:
	bash scripts/e2e.sh

e2e-native:
	bash scripts/e2e-native.sh

fmt:
	gofmt -w cmd internal migrations integration

build:
	go build ./...
	cd web && npm run build
