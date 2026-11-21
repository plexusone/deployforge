.PHONY: build test lint schema

# build writes the operator's binary and the linux/amd64 build a deploy
# uploads to the host.
build:
	go build -o bin/deployforge ./cmd/deployforge
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/deployforge-linux-amd64 ./cmd/deployforge

test:
	go test ./...

lint:
	golangci-lint run

schema:
	go generate ./schema
