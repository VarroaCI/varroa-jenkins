.PHONY: lint test build build-cli docker-build clean frontend-install frontend-dev frontend-build frontend-docker-build frontend-test generate-crds check-crds generate-proto check-proto generate-client check-client challenge-bundle challenge-bundle-check publish-challenge-bundle localdev localdev-down localdev-images localdev-controller

GIT_SHA=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_BRANCH=$(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")
IMAGE_NAME=varroa-jenkins
FRONTEND_IMAGE_NAME=varroa-frontend

lint:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace \
		golangci/golangci-lint:latest-alpine golangci-lint run --timeout 5m ./...

test:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace \
		golang:1.26 go test -v -race -count=1 ./...

build:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(shell git rev-parse --short HEAD)" -o bin/varroa-operator ./cmd/operator/
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/varroa-gateway ./cmd/gateway/
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(shell git rev-parse --short HEAD)" -o bin/varroa-bff ./cmd/bff/
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/varroa-mite ./cmd/mite/
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/varroa-updatecenter ./cmd/updatecenter/

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
	rm -f charts/varroa/crds/*.yaml
	docker run --rm --user "$(shell id -u):$(shell id -g)" \
		-e GOCACHE=/workspace/.cache/go-build \
		-e GOMODCACHE=/workspace/.cache/go-mod \
		-v "$(CURDIR):/workspace" -w /workspace \
		golang:1.26 go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.0 \
		crd paths="./api/..." output:crd:dir=./charts/varroa/crds

check-crds:
	$(MAKE) generate-crds
	git diff --exit-code charts/varroa/crds/ && \
		{ test -z "$$(git status --porcelain charts/varroa/crds/)" || exit 1; }

generate-proto:
	go generate ./internal/mite/proto/mitev1/

check-proto:
	$(MAKE) generate-proto
	git diff --exit-code internal/mite/proto/mitev1/mitev1_gen.go

generate-plugin-lock:
	bash hack/gen-plugin-lock.sh

clean:
	rm -rf bin/ frontend/dist/

# Local dev environment on kind (docs/install/local-development.md)
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
	git diff --exit-code -- api/openapi pkg/client

challenge-bundle:
	go run ./internal/jenkins/items/cmd/challengegen

challenge-bundle-check: challenge-bundle
	git diff --exit-code internal/jenkins/items/testdata/challenge/bundle-challenge

publish-challenge-bundle: challenge-bundle
	rm -rf $${VARROA_CONTROLLERS_REPO:-$$HOME/code/varroa-jenkins-controllers}/bundle-challenge
	cp -r internal/jenkins/items/testdata/challenge/bundle-challenge $${VARROA_CONTROLLERS_REPO:-$$HOME/code/varroa-jenkins-controllers}/bundle-challenge
