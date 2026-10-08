.PHONY: build build-clean build-clean-all build-freshness test test-docs test-extension test-github-e2e test-herdr-e2e test-cmux-e2e test-notification-macos-live install

build:
	go build -o bin/shephrd.tmp ./cmd/shephrd
	go build -o bin/shephrd-terminal-herdr.tmp ./cmd/shephrd-terminal-herdr
	go build -o bin/shephrd-terminal-cmux.tmp ./cmd/shephrd-terminal-cmux
	go build -o bin/shephrd-notification-macos.tmp ./cmd/shephrd-notification-macos
	go build -o bin/shephrd-repository-scanner.tmp ./cmd/shephrd-repository-scanner
	go build -o bin/shephrd-github-observer.tmp ./cmd/shephrd-github-observer
	go build -o bin/shephrd-delivery-webhook.tmp ./cmd/shephrd-delivery-webhook
	mv bin/shephrd.tmp bin/shephrd
	mv bin/shephrd-terminal-herdr.tmp bin/shephrd-terminal-herdr
	mv bin/shephrd-terminal-cmux.tmp bin/shephrd-terminal-cmux
	mv bin/shephrd-notification-macos.tmp bin/shephrd-notification-macos
	mv bin/shephrd-repository-scanner.tmp bin/shephrd-repository-scanner
	mv bin/shephrd-github-observer.tmp bin/shephrd-github-observer
	mv bin/shephrd-delivery-webhook.tmp bin/shephrd-delivery-webhook

build-clean:
	scripts/build-clean.sh cli

build-clean-all:
	scripts/build-clean.sh all

build-freshness:
	go build -o bin/shephrd-freshness.tmp ./cmd/shephrd-freshness
	mv bin/shephrd-freshness.tmp bin/shephrd-freshness

test:
	go test ./...

test-docs:
	go test ./internal/cli -run '^TestDocumentation$$' -count=1

test-extension:
	go test ./internal/extension ./internal/driverdelivery/... ./internal/notification/... ./internal/repository/discovery ./internal/repository/scanner ./internal/forge/github ./internal/terminal
	node --test tests/pi/shephrd-wake.test.ts internal/pibridge/shephrd-herdr-bridge.test.mjs

test-github-e2e:
	SHEPHRD_REAL_GITHUB_OBSERVER_E2E=1 go test ./internal/forge/github -run '^TestRealGitHubObserverReadProbeIsOptIn$$' -count=1 -v

test-herdr-e2e:
	SHEPHRD_REAL_HERDR_E2E=1 SHEPHRD_REAL_HERDR_PI_E2E=1 SHEPHRD_REAL_HERDR_CLAUDE_E2E=1 SHEPHRD_REAL_HERDR_EXTENSION_E2E=1 go test ./internal/terminal ./internal/control ./internal/cli -run 'TestLiveHerdrTabIsOptIn|TestRealInteractivePiHerdrWorkerIsOptIn|TestRealInteractivePiRepairHerdrIsOptIn|TestRealInteractiveClaudeHerdrWorkerIsOptIn|TestLiveHerdrSubDriverPresentationIsOptIn' -count=1 -v

test-cmux-e2e:
	SHEPHRD_REAL_CMUX_E2E=1 SHEPHRD_REAL_CMUX_AGENTS_E2E=1 go test ./internal/control -run '^TestRealCmux(MVP|AgentReleaseMatrix)IsOptIn$$' -count=1 -v
	SHEPHRD_REAL_CMUX_E2E=1 SHEPHRD_REAL_CMUX_EXTENSION_E2E=1 go test ./internal/control -run '^TestRealCmuxMVPIsOptIn$$' -count=1 -v

test-notification-macos-live:
	SHEPHRD_REAL_MACOS_NOTIFICATION_E2E=1 go test ./internal/notification/macos -run '^TestLiveMacOSNotificationIsOptIn$$' -count=1 -v

install:
	go install ./cmd/shephrd ./cmd/shephrd-terminal-herdr ./cmd/shephrd-terminal-cmux ./cmd/shephrd-notification-macos ./cmd/shephrd-repository-scanner ./cmd/shephrd-github-observer ./cmd/shephrd-delivery-webhook
