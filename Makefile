# Athanor is a Go server and a frontend project. `make` builds both, in the
# order that matters: the bundle is emitted into pkg/server/web_dist, which the
# Go binary embeds, so a Go build that ran first would carry the previous one.

.PHONY: all ui server test lint clean

all: ui server

## ui: build the frontend into the directory the server embeds
ui:
	cd web && pnpm install --frozen-lockfile && pnpm build

## server: build the binary (carries whatever web_dist currently holds)
server:
	go build ./...

## test: the gates — race detector on, vet included
test:
	go vet ./...
	go test -race ./...
	cd web && pnpm exec tsc -b

## lint: formatting is a gate, not a preference
lint:
	@test -z "$$(gofmt -l .)" || { echo "gofmt:"; gofmt -l .; exit 1; }

## clean: drop build products
clean:
	rm -rf pkg/server/web_dist web/node_modules
