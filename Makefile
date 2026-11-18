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
	package build release-upload release-github e2e-up e2e e2e-down e2e-auto e2e-status e2e-pushed e2e-session

help:
	@echo "make dev              open a shell in the development container"
	@echo "make dev-run CMD=...  run a command in the development container"
	@echo "make check            lint, template lint and tests with coverage thresholds"
	@echo "make test             tests only"
	@echo "make package          build/lambda.zip with the service code"
	@echo "make build VERSION=x.y.z            release files in build/release/v<version>"
	@echo "make release-upload VERSION=x.y.z TESTED=<sha> [RELEASE_PROFILE=...]   templates and code to the release buckets"
	@echo "make release-github VERSION=x.y.z TESTED=<sha> TYPE=security|fix|feature   GitHub release with the executables"
	@echo "make e2e-up / e2e / e2e-down   end-to-end tests in a real test organization"
	@echo "                      (PROFILE, SERVICE_ACCOUNT, TARGET_ACCOUNT, TEST_USER)"
	@echo "make e2e-auto         e2e-up, e2e and always e2e-down; posts the e2e status on the pushed HEAD"
	@echo "make e2e-status STATE=success|failure   post the e2e status after the manual checks"

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
	cd client && go test -count=1 -coverprofile=../build/client.cover ./internal/...
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
	cd client && go test -count=1 ./...
endif

package:
	@mkdir -p build
	cd service && uv run python -c "import sys; sys.path.insert(0, 'tests/e2e'); \
		import harness, pathlib; pathlib.Path('../build/lambda.zip').write_bytes(harness.package())"
	@echo "build/lambda.zip"

# Release. The build runs in the container (Go). Publishing runs in the release workflow
# (.github/workflows/release.yml), or by hand on a host with aws and gh, from a clean main
# whose tree has a successful e2e commit status (TESTED: the commit that carries it).
build: dev-image
	@test -n '$(VERSION)' || { echo 'usage: make build VERSION=x.y.z' >&2; exit 2; }
	$(DOCKER_RUN) dev/build.sh '$(VERSION)' build/release

release-upload:
	dev/release.sh upload '$(VERSION)' '$(TESTED)' $(if $(RELEASE_PROFILE),'$(RELEASE_PROFILE)')

release-github:
	dev/release.sh github '$(VERSION)' '$(TESTED)' '$(TYPE)'

# End-to-end tests create only elevate-e2e* resources. Always finish with e2e-down.
# The approval by a second user is opt-in (manual login): E2E_APPROVAL=1 make e2e-up e2e.
# Manual UI checks with one user per role (approver, requester, auditor): E2E_USERS=1 make e2e-up.
# A new stack is installed with the client, built in the container (Go is only there).
# Every e2e target starts with a live session: an expired token fails before anything is made.
e2e-session:
	@aws sts get-caller-identity --profile "$$PROFILE" >/dev/null 2>&1 || { echo "e2e: AWS session expired: aws sso login --profile $$PROFILE" >&2; exit 1; }

e2e-up: e2e-session dev-image
	$(DOCKER_RUN) bash -c 'cd client && CGO_ENABLED=0 go build -o ../build/aws-elevate ./cmd/aws-elevate'
	cd service && uv run python tests/e2e/harness.py up

# The CLI tests run the real executable, built in the container (Go is only there).
e2e: e2e-session dev-image
	$(DOCKER_RUN) bash -c 'cd client && CGO_ENABLED=0 go build -o ../build/aws-elevate ./cmd/aws-elevate'
	cd service && ELEVATE_E2E=1 uv run pytest tests/e2e -v -s -p no:cacheprovider

# The stack goes with aws-elevate uninstall, built in the container like for e2e-up.
e2e-down: e2e-session dev-image
	$(DOCKER_RUN) bash -c 'cd client && CGO_ENABLED=0 go build -o ../build/aws-elevate ./cmd/aws-elevate'
	cd service && uv run python tests/e2e/harness.py down

# The end-to-end outcome is a commit status named e2e on the pushed HEAD: the release
# workflow publishes only a tree whose commit carries a successful one.
e2e-pushed:
	@test -z "$$(git status --porcelain)" || { echo 'e2e: commit or remove changes first: the status names the commit' >&2; exit 1; }
	@test -n "$$(git branch -r --contains HEAD)" || { echo 'e2e: push HEAD first: GitHub keeps the status on the commit' >&2; exit 1; }

e2e-status: e2e-pushed
	@case '$(STATE)' in success|failure) ;; *) echo 'usage: make e2e-status STATE=success|failure' >&2; exit 2 ;; esac
	gh api -X POST "repos/{owner}/{repo}/statuses/$$(git rev-parse HEAD)" -f state='$(STATE)' \
		-f context=e2e -f description='$(or $(DESCRIPTION),end-to-end tests on real AWS)' >/dev/null
	@echo "e2e status $(STATE) on $$(git rev-parse --short HEAD)"

# Unattended run for small changes and hotfixes: no second-user approval, no UI check.
# The org is always torn down.
e2e-auto: e2e-pushed e2e-session
	@if $(MAKE) e2e-up e2e; then outcome=success; else outcome=failure; fi; \
	$(MAKE) e2e-down || outcome=failure; \
	$(MAKE) e2e-status STATE=$$outcome DESCRIPTION='make e2e-auto'; \
	[ $$outcome = success ]
