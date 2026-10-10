FROM golang:1.26-bookworm AS source
WORKDIR /app
COPY go.* ./
RUN go mod download
COPY . .

FROM source AS test
CMD ["go", "test", "-race", "-count=1", "-v", "./..."]

FROM source AS build
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -o /out/mock ./cmd/mock && \
    CGO_ENABLED=0 go build -o /out/migrate ./cmd/migrate

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
USER 65532:65532
CMD ["api"]
