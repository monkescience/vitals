SHELL := /bin/bash

test:
	go test ./...

lint:
	golangci-lint run
