.PHONY: build test lint schema

# build writes the operator's binary and the linux/amd64 build a deploy
# uploads to the host.
build:
	go build -o bin/deployforge ./cmd/deployforge
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/deployforge-linux-amd64 ./cmd/deployforge

test:
	go test ./...

# lint runs golangci-lint with the organization's shared configuration,
# the one CI checks out, so formatters such as gofmt run here as they do
# there. LINT_CONFIG_REPO is a checkout of the organization's .github
# repository; the configuration is read from its main branch.
LINT_CONFIG_REPO ?= ../.github
LINT_CONFIG := $(or $(TMPDIR),/tmp/)deployforge-golangci-shared.yaml

lint:
	git -C $(LINT_CONFIG_REPO) fetch --quiet origin main
	git -C $(LINT_CONFIG_REPO) show origin/main:.golangci.yaml > $(LINT_CONFIG)
	golangci-lint run --config $(LINT_CONFIG) ./...
	rm -f $(LINT_CONFIG)

schema:
	go generate ./schema
