.DEFAULT_GOAL := help

CHARTS_DIR    := charts
OPERATOR_DIR  := operator
KIND_CLUSTER  ?= proxysql-v2
KIND_VERSION  ?= v1.31.0

.PHONY: help
help: ## show available targets
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## ---- charts ----

.PHONY: lint
lint: ## helm lint all charts that have a Chart.yaml
	@found=0; for c in $(CHARTS_DIR)/*/; do \
	  if [ -f "$$c/Chart.yaml" ]; then found=1; helm lint "$$c" || exit 1; fi; \
	done; \
	[ $$found -eq 1 ] || echo "(no charts with Chart.yaml yet - Phase 1)"

.PHONY: template
template: ## helm template all charts (sanity render)
	@for c in $(CHARTS_DIR)/*/; do \
	  if [ -f "$$c/Chart.yaml" ]; then echo "==> $$c"; helm template test "$$c" >/dev/null || exit 1; fi; \
	done

.PHONY: kubeconform
kubeconform: ## render charts and validate against k8s schemas
	@command -v kubeconform >/dev/null || { echo "install kubeconform: https://github.com/yannh/kubeconform"; exit 1; }
	@for c in $(CHARTS_DIR)/*/; do \
	  if [ -f "$$c/Chart.yaml" ]; then echo "==> $$c"; helm template test "$$c" | kubeconform -strict -summary || exit 1; fi; \
	done

## ---- operator ----

.PHONY: operator-build
operator-build: ## build operator binary
	@if [ -f $(OPERATOR_DIR)/go.mod ]; then cd $(OPERATOR_DIR) && go build ./...; else echo "(operator not scaffolded yet - Phase 3)"; fi

.PHONY: operator-test
operator-test: ## run operator unit tests
	@if [ -f $(OPERATOR_DIR)/go.mod ]; then cd $(OPERATOR_DIR) && go test ./...; else echo "(operator not scaffolded yet - Phase 3)"; fi

## operator image build/publish.
##
## IMG / VERSION / COMMIT override the tag and ldflag-injected build metadata.
## PLATFORMS controls the multi-arch target list (only used by operator-image-multi).
IMG       ?= ghcr.io/proxysql/proxysql-operator:dev
VERSION   ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: sync-crds
sync-crds: ## regenerate CRDs and copy them into the proxysql-operator chart
	@if [ ! -f $(OPERATOR_DIR)/Makefile ]; then echo "(operator not scaffolded yet)"; exit 0; fi
	$(MAKE) -C $(OPERATOR_DIR) manifests
	cp $(OPERATOR_DIR)/config/crd/bases/proxysql.com_proxysqlclusters.yaml $(CHARTS_DIR)/proxysql-operator/crds/proxysqlcluster.yaml
	cp $(OPERATOR_DIR)/config/crd/bases/proxysql.com_proxysqlconfigs.yaml  $(CHARTS_DIR)/proxysql-operator/crds/proxysqlconfig.yaml

.PHONY: operator-image
operator-image: ## build operator image for the local arch and load into docker
	@if [ ! -f $(OPERATOR_DIR)/Dockerfile ]; then echo "(operator Dockerfile not present yet - Phase 3)"; exit 0; fi
	cd $(OPERATOR_DIR) && docker buildx build --load \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
	  -t $(IMG) .

.PHONY: operator-image-multi
operator-image-multi: ## build+push operator image for $(PLATFORMS) — requires a buildx builder and registry login
	@if [ ! -f $(OPERATOR_DIR)/Dockerfile ]; then echo "(operator Dockerfile not present yet - Phase 3)"; exit 0; fi
	cd $(OPERATOR_DIR) && docker buildx build --push --platform=$(PLATFORMS) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
	  -t $(IMG) .

.PHONY: operator-image-kind
operator-image-kind: operator-image ## build the image and load it into the local kind cluster
	kind load docker-image $(IMG) --name $(KIND_CLUSTER)

## ---- local cluster ----

.PHONY: kind-up
kind-up: ## create a local kind cluster for development
	kind create cluster --name $(KIND_CLUSTER) --image kindest/node:$(KIND_VERSION)

.PHONY: kind-down
kind-down: ## delete the local kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: e2e
e2e: ## run the operator end-to-end test on a kind cluster (set KEEP_CLUSTER=1 to retain)
	./test/e2e/run.sh

## ---- aggregate ----

.PHONY: test
test: lint template operator-test ## run all local checks
