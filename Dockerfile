# Build: generated code (sqlc, templ) and the CSS artifact are committed,
# so the image needs only the Go toolchain.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /crashcart ./cmd/crashcart

# Run: static binary, no shell. Migrations run at startup.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /crashcart /crashcart
EXPOSE 8080
ENTRYPOINT ["/crashcart"]
CMD ["serve"]
