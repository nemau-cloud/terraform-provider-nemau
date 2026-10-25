BINARY  ?= terraform-provider-nemau
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet fmt tidy check fmt-tf
build:
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) .
test:
	go test ./...
vet:
	go vet ./...
fmt:
	gofmt -w .
# The examples are executable documentation: they must stay formatted.
fmt-tf:
	terraform fmt -recursive examples
tidy:
	go mod tidy
check: fmt vet test
