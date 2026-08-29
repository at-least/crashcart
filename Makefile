.PHONY: build run generate sqlc templ test test-db css docker

BIN     := bin/crashcart
PKG     := ./cmd/crashcart
IMAGE   ?= crashcart:latest

build: generate
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) $(PKG)

run: generate
	go run $(PKG) serve

generate: sqlc templ

sqlc:
	sqlc generate

templ:
	templ generate

test:
	go vet ./...
	go test ./...

# DB-backed tests need a Postgres (16+), e.g.
#   docker run -d --name crashcart-test-pg -e POSTGRES_PASSWORD=crashcart -e POSTGRES_USER=crashcart \
#     -e POSTGRES_DB=crashcart -p 127.0.0.1:55432:5432 postgres:16-alpine
#   TEST_DATABASE_URL='postgres://crashcart:crashcart@127.0.0.1:55432/crashcart?sslmode=disable' make test-db
test-db:
	@test -n "$(TEST_DATABASE_URL)" || (echo "TEST_DATABASE_URL is required" >&2; exit 1)
	go vet ./...
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./...

# Rebuilds the committed CSS artifact (needs `npm install`).
css:
	npx @tailwindcss/cli -i internal/web/styles/app.css -o internal/web/assets/app.css --minify

docker:
	docker build -t $(IMAGE) .
