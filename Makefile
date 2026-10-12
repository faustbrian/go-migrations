GOLIB ?= golib

.PHONY: source-docs-check check check-full ci cohesion inventory repository-check specification-check specification-check-online engine-boundary

check: source-docs-check
	./scripts/check-engine-boundary.sh
	$(GOLIB) check --local --all

check-full: source-docs-check
	$(GOLIB) check --all

ci: source-docs-check
	$(GOLIB) repository check
	$(GOLIB) cohesion check
	./scripts/check-engine-boundary.sh
	$(GOLIB) check --local --all

engine-boundary:
	./scripts/check-engine-boundary.sh

specification-check:
	$(GOLIB) specification check

specification-check-online:
	$(GOLIB) specification check --online

cohesion:
	$(GOLIB) cohesion check

inventory repository-check:
	$(GOLIB) repository check

source-docs-check:
	$(GOLIB) source-docs check
