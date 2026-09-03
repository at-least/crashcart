.PHONY: build run generate templ gendocs test test-db mutate css docker

BIN     := bin/crashcart
PKG     := ./cmd/crashcart
IMAGE   ?= crashcart:latest

build: generate
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) $(PKG)

run: generate
	go run $(PKG) serve

generate: templ gendocs

templ:
	templ generate

gendocs:
	go run ./cmd/gendocs

test:
	go vet ./...
	go run ./cmd/gendocs -check
	go test ./...

# DB-backed tests need a Postgres (16+), and the S3 blob-store tests a
# MinIO. If TEST_DATABASE_URL / TEST_S3_ENDPOINT aren't already set,
# cmd/testpg and cmd/testminio provision them via Docker (reusable named
# containers "crashcart-testpg" / "crashcart-testminio" — docker rm -f to
# reset).
test-db:
	go vet ./...
	TEST_DATABASE_URL="$${TEST_DATABASE_URL:-$$(go run ./cmd/testpg)}" TEST_S3_ENDPOINT="$${TEST_S3_ENDPOINT:-$$(go run ./cmd/testminio)}" go test ./...

# Mutation testing (go-gremlins/gremlins: go install
# github.com/go-gremlins/gremlins/cmd/gremlins@latest). Runs the covering
# tests once per mutant, so run it in its own git worktree - not the
# working tree you're actively editing, since it mutates the tree it
# tests. go clean -testcache first: a warm cache makes gremlins measure a
# too-fast baseline test time and time out almost every mutant against it.
mutate:
	go clean -testcache
	TEST_DATABASE_URL="$${TEST_DATABASE_URL:-$$(go run ./cmd/testpg)}" TEST_S3_ENDPOINT="$${TEST_S3_ENDPOINT:-$$(go run ./cmd/testminio)}" gremlins unleash --timeout-coefficient 8 .

# Rebuilds the committed CSS artifact with the locally installed Tailwind
# (`npm install` first; no npx, so nothing is fetched at build time).
css:
	node_modules/.bin/tailwindcss -i internal/web/styles/app.css -o internal/web/assets/app.css --minify

docker:
	docker build -t $(IMAGE) .
