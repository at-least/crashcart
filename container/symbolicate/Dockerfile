# The dSYM symbolication sidecar: the crashcart binary's `symbolicate`
# subcommand next to llvm-symbolizer (the one thing the main image does not
# carry — LLVM is a few hundred MB, and only iOS / macOS projects need it).
# Build from the repository root:
#   docker build -f container/symbolicate/Dockerfile -t crashcart-symbolicate .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /crashcart ./cmd/crashcart

FROM debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends llvm && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /crashcart /crashcart
ENV SYMBOLICATE_CACHE_DIR=/var/cache/crashcart-symbols
RUN mkdir -p /var/cache/crashcart-symbols && chown nobody /var/cache/crashcart-symbols
USER nobody
EXPOSE 8080
ENTRYPOINT ["/crashcart"]
CMD ["symbolicate"]
