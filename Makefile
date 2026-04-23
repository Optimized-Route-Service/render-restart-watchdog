.PHONY: fmt vet test test-race build build-docker run clean

BINARY := render-watchdog
PKG := ./...

fmt:
	gofmt -s -w .

vet:
	go vet $(PKG)

test:
	go test $(PKG)

test-race:
	go test -race -count=1 $(PKG)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/$(BINARY) ./cmd/render-watchdog

build-docker:
	docker build -t xrouten-render-watchdog:local .

run: build
	./bin/$(BINARY)

clean:
	rm -rf bin
