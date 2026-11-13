FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ghpool ./cmd/ghpool

# Static binary on a base with CA certificates and nothing else.
FROM gcr.io/distroless/static-debian12
COPY --from=build /ghpool /ghpool
ENTRYPOINT ["/ghpool"]
