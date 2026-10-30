.PHONY: engine-boundary

engine-boundary:
	./scripts/check-engine-boundary.sh

.PHONY: interop

# The v4/v5 harness is a separate testdata module so v4 and its Goose and
# database/sql graph never enter the root module.
interop:
	cd testdata/interop && GOWORK=off go test -tags integration -count=1 -timeout 15m ./...
