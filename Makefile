CONTROLLER_GEN ?= $(CURDIR)/bin/controller-gen
COPA_SRC ?= $(HOME)/go/src/github.com/project-copacetic/copacetic
REGISTRY ?= localhost:5001
IN_CLUSTER_REGISTRY ?= kind-registry:5000
TAG ?= dev
KCTX ?= kind-copa

.PHONY: all
all: generate build test

.PHONY: generate
generate: ## Generate deepcopy code and CRDs.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd

.PHONY: build
build:
	go build -o bin/copa-rollout-controller ./cmd/controller
	go build -o bin/copa-worker ./cmd/worker

.PHONY: test
test:
	go test ./...

.PHONY: lint
lint:
	go vet ./...

.PHONY: images
images: ## Build controller and worker images and push them to the local registry.
	cd $(COPA_SRC) && CGO_ENABLED=0 go build -o $(CURDIR)/bin/copa .
	CGO_ENABLED=0 GOOS=linux go build -o bin/linux/copa-rollout-controller ./cmd/controller
	CGO_ENABLED=0 GOOS=linux go build -o bin/linux/copa-worker ./cmd/worker
	cp bin/copa bin/linux/copa
	docker build -q -f Dockerfile.controller -t $(REGISTRY)/copa/copa-rollout-controller:$(TAG) bin/linux
	docker build -q -f Dockerfile.worker -t $(REGISTRY)/copa/copa-worker:$(TAG) bin/linux
	docker push -q $(REGISTRY)/copa/copa-rollout-controller:$(TAG)
	docker push -q $(REGISTRY)/copa/copa-worker:$(TAG)

.PHONY: kind
kind: ## Create the kind cluster and local registry.
	./hack/kind-up.sh

.PHONY: deploy
deploy: ## Install CRDs, admission policies and the controller into the kind cluster.
	./hack/deploy.sh

.PHONY: demo
demo:
	./hack/demo.sh
