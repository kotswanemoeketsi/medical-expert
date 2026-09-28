# Convenience targets for Linux and macOS. On Windows use .\make.ps1.

.PHONY: help build test test-integration vet migrate check run

help:
	@echo "make build              build bin/server, bin/migrate, bin/bootstrap"
	@echo "make test               unit tests (no database needed)"
	@echo "make test-integration   all tests; needs MES_TEST_DSN"
	@echo "make vet                go vet, including integration tests"
	@echo "make migrate            apply pending migrations"
	@echo "make check              exit 2 if migrations are pending"
	@echo "make run                run the API server"

build:
	go build -o bin/ ./cmd/...

test:
	go test ./...

test-integration:
	@test -n "$$MES_TEST_DSN" || (echo "set MES_TEST_DSN, e.g. mes:password@tcp(127.0.0.1:3306)/mes_test" && exit 1)
	go test -tags=integration -count=1 ./...

vet:
	go vet -tags=integration ./...

migrate:
	go run ./cmd/migrate

check:
	go run ./cmd/migrate -check

run:
	go run ./cmd/server
