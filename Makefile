.PHONY: build test lint golden-update

build:
	go build ./...

test:
	go test -race ./...

lint:
	golangci-lint run

golden-update:
	go test ./... -run TestGolden -update
