.PHONY: build run generate sqlc templ test test-db css lint docker

build: generate
	go build -o bin/crashcart ./cmd/crashcart

run:
	go run ./cmd/crashcart serve

generate: sqlc templ

sqlc:
	sqlc generate

templ:
	templ generate

test:
	go vet ./... && go test ./...

# Integration tests need a Postgres: TEST_DATABASE_URL=postgres://user:pass@host/db?sslmode=disable
test-db:
	@test -n "$(TEST_DATABASE_URL)" || (echo "TEST_DATABASE_URL is required" && exit 1)
	go test ./...

# Rebuild the committed stylesheet artifact (Tailwind v4 + shadless; needs `npm install`).
css:
	npx @tailwindcss/cli -i internal/web/styles/app.css -o internal/web/assets/app.css --minify

docker:
	docker build -t crashcart .
