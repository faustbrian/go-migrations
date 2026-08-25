SHELL := /usr/bin/env bash

.PHONY: check ci inventory repository-check

check:
	./scripts/with-disposable-go-cache.sh ./scripts/run-modules.sh check --all

ci: repository-check check

inventory repository-check:
	./scripts/repository-check.sh
