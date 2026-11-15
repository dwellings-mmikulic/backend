.PHONY: build test test-quick vet run up down logs tidy swagger

build:
	go build -o bin/server ./cmd/server

# Every test, none skipped: starts a throwaway Postgres, needs docker.
test:
	./scripts/test-all.sh

# Only the tests that need nothing but Go; the database tests are skipped.
test-quick:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

run:
	go run ./cmd/server

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f app

swagger:
	go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/server/main.go -o docs --parseInternal
