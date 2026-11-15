.DEFAULT_GOAL := help

IMAGE := aws-elevate-dev
SERVICE_COVERAGE := 90
CLIENT_COVERAGE := 80
TEMPLATES := $(wildcard infra/*.yaml)

# ~/.aws is mounted read-write: renewing the SSO token writes to its cache.
# The named volume keeps Go and uv downloads between runs.
DOCKER_RUN = docker run --rm -i $(shell [ -t 0 ] && echo -t) \
	-v "$(CURDIR)":/work \
	-v "$(HOME)/.aws":/home/dev/.aws \
	-v aws-elevate-cache:/home/dev/.cache \
	$(IMAGE)

.PHONY: help dev-image dev dev-run check check-service check-client test test-service test-client \
	package e2e-up e2e e2e-down

help:
	@echo "make dev              open a shell in the development container"
	@echo "make dev-run CMD=...  run a command in the development container"
	@echo "make check            lint, template lint and tests with coverage thresholds"
	@echo "make test             tests only"
	@echo "make package          build/lambda.zip with the service code"
	@echo "make e2e-up / e2e / e2e-down   end-to-end tests in a real test organization"
	@echo "                      (PROFILE, SERVICE_ACCOUNT, TARGET_ACCOUNT, TEST_USER)"

dev-image:
	docker build --build-arg UID=$(shell id -u) --build-arg GID=$(shell id -g) -t $(IMAGE) dev

dev: dev-image
	$(DOCKER_RUN) bash

dev-run: dev-image
	@test -n '$(CMD)' || { echo 'usage: make dev-run CMD="..."' >&2; exit 2; }
	$(DOCKER_RUN) bash -c '$(CMD)'

check: check-service check-client

check-service:
	cd service && uv run ruff check . && uv run ruff format --check .
ifneq ($(TEMPLATES),)
	cd service && uv run cfn-lint $(addprefix ../,$(TEMPLATES))
else
	@echo "no templates in infra/, skipping cfn-lint"
endif
	cd service && uv run pytest --cov --cov-report=term-missing --cov-fail-under=$(SERVICE_COVERAGE)

check-client:
ifeq ($(wildcard client/go.mod),)
	@echo "warning: client/go.mod not found, skipping client checks"
else
	@unformatted="$$(cd client && gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt: these files need formatting:"; echo "$$unformatted"; exit 1; fi
	cd client && go vet ./...
	@mkdir -p build
	cd client && go test -coverprofile=../build/client.cover ./internal/...
	@cd client && go tool cover -func=../build/client.cover | awk -v min=$(CLIENT_COVERAGE) \
		'/^total:/ { pct = $$3; sub("%", "", pct); print "client coverage " pct "%"; \
		if (pct + 0 < min) { print "below the " min "% threshold"; exit 1 } }'
endif

test: test-service test-client

test-service:
	cd service && uv run pytest

test-client:
ifeq ($(wildcard client/go.mod),)
	@echo "warning: client/go.mod not found, skipping client tests"
else
	cd client && go test ./...
endif

package:
	@mkdir -p build
	cd service && uv run python -c "import sys; sys.path.insert(0, 'tests/e2e'); \
		import harness, pathlib; pathlib.Path('../build/lambda.zip').write_bytes(harness.package())"
	@echo "build/lambda.zip"

# End-to-end tests create only elevate-e2e* resources. Always finish with e2e-down.
e2e-up:
	cd service && uv run python tests/e2e/harness.py up

e2e:
	cd service && ELEVATE_E2E=1 uv run pytest tests/e2e -v -s -p no:cacheprovider

e2e-down:
	cd service && uv run python tests/e2e/harness.py down
