.PHONY: build run generate sqlc templ gendocs test test-db mutate css docker

BIN     := bin/crashcart
PKG     := ./cmd/crashcart
IMAGE   ?= crashcart:latest

build: generate
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) $(PKG)

run: generate
	go run $(PKG) serve

generate: sqlc templ gendocs

sqlc:
	sqlc generate

templ:
	templ generate

gendocs:
	go run ./cmd/gendocs

test:
	go vet ./...
	go run ./cmd/gendocs -check
	go test ./...

# DB-backed tests need a Postgres (16+), e.g.
#   docker run -d --name crashcart-test-pg -e POSTGRES_PASSWORD=crashcart -e POSTGRES_USER=crashcart \
#     -e POSTGRES_DB=crashcart -p 127.0.0.1:55432:5432 postgres:16-alpine
#   TEST_DATABASE_URL='postgres://crashcart:crashcart@127.0.0.1:55432/crashcart?sslmode=disable' make test-db
test-db:
	@test -n "$(TEST_DATABASE_URL)" || (echo "TEST_DATABASE_URL is required" >&2; exit 1)
	go vet ./...
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./...

# Mutation testing (go-gremlins/gremlins: go install
# github.com/go-gremlins/gremlins/cmd/gremlins@latest). Runs the covering
# tests once per mutant, so run it in its own git worktree against its own
# disposable Postgres (a different port from the one make test-db uses) -
# not the working tree you're actively editing, and not the same database:
# pg_notify channels are per-database, not per-schema (see internal/testdb),
# so two full test-suite runs sharing one Postgres can cross-signal on
# Listener-keyed tests. go clean -testcache first: a warm cache makes
# gremlins measure a too-fast baseline test time and time out almost every
# mutant against it.
mutate:
	@test -n "$(TEST_DATABASE_URL)" || (echo "TEST_DATABASE_URL is required" >&2; exit 1)
	go clean -testcache
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" gremlins unleash --timeout-coefficient 8 .

# Rebuilds the committed CSS artifact with the locally installed Tailwind
# (`npm install` first; no npx, so nothing is fetched at build time).
css:
	node_modules/.bin/tailwindcss -i internal/web/styles/app.css -o internal/web/assets/app.css --minify

docker:
	docker build -t $(IMAGE) .
