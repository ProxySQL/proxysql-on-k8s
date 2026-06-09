.DEFAULT_GOAL := help

CHARTS_DIR    := charts
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
	[ $$found -eq 1 ] || echo "(no charts with Chart.yaml found)"

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

## ---- local cluster ----

.PHONY: kind-up
kind-up: ## create a local kind cluster for development
	kind create cluster --name $(KIND_CLUSTER) --image kindest/node:$(KIND_VERSION)

.PHONY: kind-down
kind-down: ## delete the local kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

## ---- aggregate ----

.PHONY: test
test: lint template ## run all local checks
