GO ?= go
PYTHON ?= python3
GO_FILES := $(shell find cmd internal -type f -name '*.go' -print 2>/dev/null)

.PHONY: all build build-go companion test test-go test-android check fmt-check vet scripts-check skill-check payload-check licenses clean

all: check test build

build: build-go companion

build-go:
	mkdir -p bin
	$(GO) build -trimpath -o bin/wechatcopilot ./cmd/wechatcopilot

companion:
	cd android && ./gradlew --no-daemon :companion:assembleDebug

test: test-go test-android

test-go:
	$(GO) test ./...

test-android:
	cd android && ./gradlew --no-daemon :companion:testDebugUnitTest

check: fmt-check vet scripts-check skill-check payload-check

fmt-check:
	test -z "$$(gofmt -l $(GO_FILES))"

vet:
	$(GO) vet ./...

scripts-check:
	$(PYTHON) -c 'compile(open("deploy/wechat/ui_driver.py", "rb").read(), "ui_driver.py", "exec")'
	$(PYTHON) -m unittest -v deploy/wechat/test_ui_driver.py
	@set -e; for script in deploy/wechat/*.sh; do sh -n "$$script"; done

skill-check:
	$(PYTHON) scripts/validate_skill.py skills/wechatcopilot

payload-check:
	$(PYTHON) scripts/check_repository_payloads.py

licenses:
	@set -e; \
	report="$$(mktemp)"; \
	trap 'rm -f "$$report"' EXIT; \
	$(GO) run github.com/google/go-licenses/v2@v2.0.1 report ./cmd/wechatcopilot >"$$report"; \
	$(PYTHON) scripts/check_dependency_licenses.py "$$report"; \
	cd android && ./gradlew --no-daemon :companion:licensee

clean:
	$(GO) clean
	cd android && ./gradlew --no-daemon clean
