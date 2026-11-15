.DEFAULT_GOAL := help

IMAGE := aws-elevate-dev

# ~/.aws is mounted read-write: renewing the SSO token writes to its cache.
# The named volume keeps Go and uv downloads between runs.
DOCKER_RUN = docker run --rm -i $(shell [ -t 0 ] && echo -t) \
	-v "$(CURDIR)":/work \
	-v "$(HOME)/.aws":/home/dev/.aws \
	-v aws-elevate-cache:/home/dev/.cache \
	$(IMAGE)

.PHONY: help dev-image dev dev-run

help:
	@echo "make dev              open a shell in the development container"
	@echo "make dev-run CMD=...  run a command in the development container"

dev-image:
	docker build --build-arg UID=$(shell id -u) --build-arg GID=$(shell id -g) -t $(IMAGE) dev

dev: dev-image
	$(DOCKER_RUN) bash

dev-run: dev-image
	@test -n '$(CMD)' || { echo 'usage: make dev-run CMD="..."' >&2; exit 2; }
	$(DOCKER_RUN) bash -c '$(CMD)'
