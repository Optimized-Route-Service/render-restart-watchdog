FROM golang:1.24-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/render-watchdog \
    ./cmd/render-watchdog

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/render-watchdog /render-watchdog

USER nonroot:nonroot

ENTRYPOINT ["/render-watchdog"]
