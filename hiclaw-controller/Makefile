.PHONY: build build-controller build-cli clean test test-unit test-integration test-all generate controller-gen

# --- Build ---

build: build-controller build-cli

build-controller:
	CGO_ENABLED=1 go build -o bin/hiclaw-controller ./cmd/controller/

build-cli:
	CGO_ENABLED=0 go build -o bin/hiclaw ./cmd/hiclaw/

clean:
	rm -rf bin/

# --- Test ---

GOBIN ?= $(shell go env GOPATH)/bin
ENVTEST ?= $(GOBIN)/setup-envtest
ENVTEST_K8S_VERSION ?= 1.31.0

# Detect architecture for envtest binaries
UNAME_ARCH := $(shell uname -m)
ifeq ($(UNAME_ARCH),arm64)
  ENVTEST_ARCH ?= arm64
else
  ENVTEST_ARCH ?= amd64
endif

KUBEBUILDER_ASSETS ?= $(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --arch=$(ENVTEST_ARCH) -p path 2>/dev/null)

test: test-unit

test-unit:
	go test ./internal/... ./cmd/...

test-integration: setup-envtest
	KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" \
	go test -tags=integration -race -count=1 -v -timeout=120s ./test/integration/...

test-all: test-unit test-integration

# --- Generate ---

CONTROLLER_GEN ?= $(GOBIN)/controller-gen

controller-gen: ## Ensure controller-gen is installed
	@test -x $(CONTROLLER_GEN) || go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

generate: controller-gen ## Regenerate deepcopy functions and sync CRDs to Helm
	$(CONTROLLER_GEN) object paths=./api/...
	cp config/crd/*.yaml ../helm/hiclaw/crds/
	@echo "==> DeepCopy regenerated, CRDs synced to Helm"

# --- Tools ---

setup-envtest:
	@test -x $(ENVTEST) || go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --arch=$(ENVTEST_ARCH) -p path >/dev/null 2>&1 || \
		$(ENVTEST) use $(ENVTEST_K8S_VERSION) --arch=$(ENVTEST_ARCH)
