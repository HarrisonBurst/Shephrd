.PHONY: build install test test-docs vet

build:
	go build -o bin/shephrd.tmp ./cmd/shephrd
	mv bin/shephrd.tmp bin/shephrd

install:
	go install ./cmd/shephrd

test:
	go test ./...

test-docs:
	go test . -run '^TestDocumentation$$' -count=1

vet:
	go vet ./...
