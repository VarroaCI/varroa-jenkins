.PHONY: lint test build build-cli docker-build clean frontend-install frontend-dev frontend-build frontend-docker-build frontend-test generate-crds check-crds check-generated-test check-comments check-outbound outbound-doc export-gate chart-test generate-proto check-proto generate-client check-client challenge-bundle challenge-bundle-check publish-challenge-bundle localdev localdev-down localdev-images localdev-controller

GIT_SHA=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_BRANCH=$(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")
IMAGE_NAME=varroa-jenkins
FRONTEND_IMAGE_NAME=varroa-frontend

# Keep this version in step with the workflow pins (see AGENTS.md > Linting).
# `latest-alpine` here would let the documented local gate drift past CI and
# report failures nobody can reproduce in Actions, or miss ones CI will catch.
GOLANGCI_LINT_VERSION=v2.12.2

lint:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace \
		golangci/golangci-lint:$(GOLANGCI_LINT_VERSION)-alpine golangci-lint run --timeout 5m ./...

test:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace \
		golang:1.26 go test -v -race -count=1 ./...

build:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(shell git rev-parse --short HEAD)" -o bin/varroa-operator ./cmd/operator/
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/varroa-gateway ./cmd/gateway/
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(shell git rev-parse --short HEAD)" -o bin/varroa-bff ./cmd/bff/
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(shell git rev-parse --short HEAD)" -o bin/varroa-mite ./cmd/mite/
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(shell git rev-parse --short HEAD)" -o bin/varroa-updatecenter ./cmd/updatecenter/
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(shell git rev-parse --short HEAD)" -o bin/varroa-recorder ./cmd/recorder/

build-cli:
	go build -ldflags "-X main.version=$$(git describe --tags --always --dirty)" -o bin/varroactl ./cmd/varroactl

docker-build:
	docker build \
		--build-arg GIT_SHA=$(GIT_SHA) \
		--build-arg GIT_BRANCH=$(GIT_BRANCH) \
		-t $(IMAGE_NAME):$(GIT_SHA) \
		-t $(IMAGE_NAME):$(GIT_BRANCH) \
		.

frontend-install:
	cd frontend && npm ci

frontend-dev:
	cd frontend && npm run dev

frontend-build:
	cd frontend && npm run build

frontend-docker-build:
	docker build \
		-t $(FRONTEND_IMAGE_NAME):$(GIT_SHA) \
		-t $(FRONTEND_IMAGE_NAME):$(GIT_BRANCH) \
		frontend/

frontend-test:
	cd frontend && npm run coverage

generate-crds:
	go generate ./api/v1alpha1/...
	rm -f charts/varroa/crds/*.yaml
	docker run --rm --user "$(shell id -u):$(shell id -g)" \
		-e GOCACHE=/workspace/.cache/go-build \
		-e GOMODCACHE=/workspace/.cache/go-mod \
		-v "$(CURDIR):/workspace" -w /workspace \
		golang:1.26 go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.0 \
		crd paths="./api/..." output:crd:dir=./charts/varroa/crds

check-crds:
	$(MAKE) generate-crds
	hack/check-generated.sh generate-crds charts/varroa/crds api/v1alpha1/teamagent_types.go

check-comments:
	go run ./hack/checkcomments .

# Fails on an unlisted host, a stale inventory row, or a page that differs
# from hack/checkoutbound/inventory.yaml.
check-outbound:
	go run ./hack/checkoutbound .

# Regenerates docs/security/outbound-connections.md from the inventory.
outbound-doc:
	go run ./hack/checkoutbound -write .

# Runs the public and site export leak gates into a scratch directory.
# The exports gate the HEAD commit, not the working tree: commit first.
export-gate:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; rc=0; \
	hack/export-public.sh "$$tmp/public" || rc=1; \
	hack/export-site.sh "$$tmp/site" || rc=1; \
	exit $$rc

# Runs every chart golden test. The NATS subchart must be fetched first:
# helm dependency build charts/varroa.
# The tests expect the release namespace "default". HELM_NAMESPACE pins it,
# because helm in a pod otherwise takes the pod's own namespace.
chart-test:
	@test -n "$$(ls charts/varroa/charts/nats-*.tgz 2>/dev/null)" || \
		{ echo "chart-test: run 'helm dependency build charts/varroa' first" >&2; exit 1; }
	@failed=""; \
	for t in charts/varroa/tests/test-*.sh; do \
		echo "=== $$t"; \
		HELM_NAMESPACE=default bash "$$t" || failed="$$failed $$t"; \
	done; \
	if [ -n "$$failed" ]; then \
		for t in $$failed; do echo "FAILED: $$t"; done; \
		exit 1; \
	fi; \
	echo "chart-test: all tests passed"

generate-proto:
	go generate ./internal/mite/proto/mitev1/

check-proto:
	$(MAKE) generate-proto
	hack/check-generated.sh generate-proto internal/mite/proto/mitev1/mitev1_gen.go

generate-plugin-lock:
	bash hack/gen-plugin-lock.sh

clean:
	rm -rf bin/ frontend/dist/

# Local dev environment on kind (docs/internal/maintainers/local-development.md)
localdev: ## Stand up the full local dev environment (kind + helm + sample controller)
	bash hack/localdev/localdev.sh up

localdev-down: ## Delete the localdev kind cluster (keeps .localdev/ certs)
	bash hack/localdev/localdev.sh down

localdev-images: ## Rebuild images, kind-load, and roll the localdev release
	bash hack/localdev/localdev.sh images

localdev-controller: ## (Re-)apply the localdev sample controller
	bash hack/localdev/localdev.sh controller

generate-client:
	go run ./hack/openapi-bundle
	go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config api/openapi/oapi-codegen.yaml api/openapi/varroa.yaml
	gofmt -w pkg/client

check-client:
	git diff --quiet -- api/openapi pkg/client || (echo "run make generate-client" && exit 1)
	$(MAKE) generate-client
	hack/check-generated.sh generate-client api/openapi pkg/client

challenge-bundle:
	go run ./internal/jenkins/items/cmd/challengegen

challenge-bundle-check: challenge-bundle
	hack/check-generated.sh challenge-bundle internal/jenkins/items/testdata/challenge/bundle-challenge

check-generated-test:
	bash hack/check-generated_test.sh

publish-challenge-bundle: challenge-bundle
	@test -n "$$VARROA_CONTROLLERS_REPO" || { echo "VARROA_CONTROLLERS_REPO must point at a CasC bundle repo checkout" >&2; exit 1; }
	rm -rf "$$VARROA_CONTROLLERS_REPO/bundle-challenge"
	cp -r internal/jenkins/items/testdata/challenge/bundle-challenge "$$VARROA_CONTROLLERS_REPO/bundle-challenge"
