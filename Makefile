SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

IMAGE       ?= kqos:dev
CLUSTER     ?= kqos
NAMESPACE   ?= kqos-system
KIND_CONFIG ?= deploy/kind-cluster.yaml
ARCH        ?= $(shell go env GOARCH)
CONTROLLER_GEN ?= ./bin/controller-gen

##@ General

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nkqos -- Kubernetes colocation resource governor\n\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

tidy: ## Tidy go modules
	go mod tidy

fmt: ## Format the source
	gofmt -w ./cmd ./pkg

vet: ## Run go vet
	go vet ./...

test: ## Run unit tests
	go test ./... -count=1

test-race: ## Run unit tests with the race detector
	go test ./... -count=1 -race

build: ## Build all three binaries into bin/
	@mkdir -p bin
	go build -o bin/kqos-agent      ./cmd/kqos-agent
	go build -o bin/kqos-controller ./cmd/kqos-controller
	go build -o bin/kqos-webhook    ./cmd/kqos-webhook

controller-gen: ## Install controller-gen into bin/
	@test -x $(CONTROLLER_GEN) || GOBIN=$(PWD)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

generate: controller-gen ## Regenerate deepcopy functions and CRD manifests
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths=./pkg/apis/...
	$(CONTROLLER_GEN) crd paths=./pkg/apis/... output:crd:artifacts:config=deploy/crds

check: fmt vet test ## Format, vet and test

##@ Cluster

kind-up: ## Create the three-node kind cluster
	kind create cluster --name $(CLUSTER) --config $(KIND_CONFIG) --wait 120s
	kubectl config use-context kind-$(CLUSTER)

kind-down: ## Delete the kind cluster
	kind delete cluster --name $(CLUSTER)

image: ## Build the container image
	docker build --build-arg TARGETARCH=$(ARCH) -t $(IMAGE) .

kind-load: image ## Build the image and load it into kind
	kind load docker-image $(IMAGE) --name $(CLUSTER)

##@ Deployment

crds: ## Apply the CustomResourceDefinitions
	kubectl apply -f deploy/crds/
	kubectl wait --for=condition=Established --timeout=60s \
		crd/noderesourceprofiles.kqos.io \
		crd/qospolicies.kqos.io \
		crd/workloadprofiles.kqos.io

deploy: crds ## Deploy every kqos component and wire up webhook TLS
	kubectl apply -f deploy/manifests/00-namespace-rbac.yaml
	kubectl apply -f deploy/manifests/20-controller.yaml
	kubectl apply -f deploy/manifests/30-webhook.yaml
	kubectl apply -f deploy/manifests/40-webhook-config.yaml
	./hack/gen-webhook-certs.sh
	kubectl apply -f deploy/manifests/10-agent.yaml
	@echo "==> waiting for components"
	kubectl -n $(NAMESPACE) rollout status deploy/kqos-controller --timeout=180s
	kubectl -n $(NAMESPACE) rollout status deploy/kqos-webhook    --timeout=180s
	kubectl -n $(NAMESPACE) rollout status ds/kqos-agent          --timeout=180s

undeploy: ## Remove kqos from the cluster
	-kubectl delete -f deploy/manifests/ --ignore-not-found
	-kubectl delete -f deploy/crds/ --ignore-not-found

redeploy: kind-load ## Rebuild the image and restart every component
	kubectl -n $(NAMESPACE) rollout restart ds/kqos-agent deploy/kqos-controller deploy/kqos-webhook
	kubectl -n $(NAMESPACE) rollout status ds/kqos-agent --timeout=180s

up: kind-up kind-load deploy ## Full path from nothing to a running cluster

##@ Demo

demo: ## Deploy the demo workloads
	kubectl apply -f examples/

demo-clean: ## Remove the demo workloads
	-kubectl delete -f examples/ --ignore-not-found

status: ## Show what kqos currently believes about the cluster
	@echo "=== node resource profiles ==="
	@kubectl get noderesourceprofiles -o wide 2>/dev/null || true
	@echo
	@echo "=== policy rollup ==="
	@kubectl get qospolicy default -o wide 2>/dev/null || true
	@echo
	@echo "=== advertised reclaimed capacity ==="
	@kubectl get nodes -o custom-columns=\
'NODE:.metadata.name,CPU:.status.allocatable.cpu,RECLAIM-CPU:.status.allocatable.kqos\.io/reclaimed-cpu,RECLAIM-MEM-MiB:.status.allocatable.kqos\.io/reclaimed-memory' 2>/dev/null || true
	@echo
	@echo "=== workload profiles ==="
	@kubectl get workloadprofiles -A -o wide 2>/dev/null || true

watch: ## Live view of the same information
	watch -n 5 $(MAKE) status

logs-agent: ## Tail the agent logs
	kubectl -n $(NAMESPACE) logs -l app.kubernetes.io/component=agent --tail=100 -f --max-log-requests=6

logs-controller: ## Tail the controller logs
	kubectl -n $(NAMESPACE) logs -l app.kubernetes.io/component=controller --tail=100 -f --max-log-requests=4

logs-webhook: ## Tail the webhook logs
	kubectl -n $(NAMESPACE) logs -l app.kubernetes.io/component=webhook --tail=100 -f --max-log-requests=4

events: ## Show kqos eviction events
	kubectl get events -A --field-selector reason=KqosEvicted --sort-by=.lastTimestamp

.PHONY: help tidy fmt vet test test-race build controller-gen generate check \
        kind-up kind-down image kind-load crds deploy undeploy redeploy up \
        demo demo-clean status watch logs-agent logs-controller logs-webhook events
