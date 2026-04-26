IMAGE_TAG ?= latest
IMG ?= ghcr.io/kraghavan/pd-ratio-coordinator:$(IMAGE_TAG)

# Go settings
GOBIN ?= $(shell go env GOPATH)/bin

.PHONY: all build test lint docker-build docker-push deploy undeploy

all: build

## Build the manager binary
build:
	go build -o bin/manager main.go

## Run unit tests
test:
	go test ./... -v -count=1

## Run linter
lint:
	$(GOBIN)/golangci-lint run ./...

## Build Docker image
docker-build:
	docker build -t $(IMG) .

## Push Docker image
docker-push:
	docker push $(IMG)

## Install CRDs into the cluster
install-crds:
	kubectl apply -f config/crd/

## Uninstall CRDs from the cluster
uninstall-crds:
	kubectl delete -f config/crd/ --ignore-not-found

## Deploy controller to cluster
deploy:
	kubectl apply -f config/rbac/
	kubectl apply -f config/manager/

## Undeploy controller from cluster
undeploy:
	kubectl delete -f config/manager/ --ignore-not-found
	kubectl delete -f config/rbac/ --ignore-not-found

## Apply sample CR
sample:
	kubectl apply -f config/samples/pdratiopolicy-sample.yaml

## Watch controller decisions in real time
watch:
	kubectl get pdratiopolicy -n llm-d -w

## Stream controller logs
logs:
	kubectl logs -n pd-ratio-coordinator-system \
	  -l control-plane=controller-manager -f

## Run locally against current kubeconfig (for development)
run:
	go run main.go \
	  --metrics-bind-address=:8080 \
	  --health-probe-bind-address=:8081

## Generate deepcopy methods (requires controller-gen)
generate:
	$(GOBIN)/controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

## Generate CRD manifests (requires controller-gen)
manifests:
	$(GOBIN)/controller-gen rbac:roleName=manager-role \
	  crd webhook paths="./..." \
	  output:crd:artifacts:config=config/crd \
	  output:rbac:artifacts:config=config/rbac

## Install controller-gen
controller-gen:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

## Format Go code
fmt:
	go fmt ./...

## Vet Go code
vet:
	go vet ./...

## Show status of controller and CRs
status:
	@echo "=== PDRatioPolicy resources ==="
	kubectl get pdratiopolicy -A
	@echo ""
	@echo "=== Controller pods ==="
	kubectl get pods -n pd-ratio-coordinator-system 2>/dev/null || true
	@echo ""
	@echo "=== Recent scale events ==="
	kubectl get events -n llm-d --field-selector reason=Scaled \
	  --sort-by='.lastTimestamp' 2>/dev/null | tail -10 || true
