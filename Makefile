.PHONY: build plugins install test test-docs test-extension vet

PLUGINS := github herdr cmux macos-notify

build:
	go build -o bin/shephrd.tmp ./cmd/shephrd
	mv bin/shephrd.tmp bin/shephrd

plugins:
	for plugin in $(PLUGINS); do go build -o plugins/$$plugin/bin/shephrd-$$plugin ./plugins/$$plugin || exit 1; done

install:
	go install ./cmd/shephrd

test:
	go test ./...

test-docs:
	go test . -run '^TestDocumentation$$' -count=1

test-extension:
	node --test plugins/pi/*.test.ts

vet:
	go vet ./...
