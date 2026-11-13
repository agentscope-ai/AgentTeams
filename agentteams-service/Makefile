GO ?= go

.PHONY: build test race test-integration test-e2e vet fmt-check run image

# Image name/tag are release inputs; the default tag is for local smoke tests.
IMAGE ?= agentteams-service:dev

build:
	$(GO) build ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race -count=1 ./...

# Requires AGENTTEAMS_TEST_RES_DSN / AGENTTEAMS_TEST_OPS_DSN (MySQL 8 or
# standalone seekdb). The tests skip when the DSNs are unset.
test-integration:
	$(GO) test -tags integration -count=1 ./test/integration/...

# Same DSN prerequisites as test-integration; builds and runs the real
# binary against a fake controller (the controller /internal/v1 routes are
# R1-2e).
test-e2e:
	$(GO) test -tags e2e -count=1 ./test/e2e/...

vet:
	$(GO) vet ./...

fmt-check:
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then echo "not gofmt-clean:"; printf '%s\n' $$files; exit 1; fi

run:
	$(GO) run ./cmd/agentteams-service $(ARGS)

# Multi-stage build (golang:1.26 -> alpine, non-root). Verified with a real
# container against MySQL 8 and the fake controller (58 §11).
image:
	docker build -t $(IMAGE) .
