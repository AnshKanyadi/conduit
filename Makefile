##
## Conduit — developer experience wrapper
##
## Usage:  make <target>
##

.DEFAULT_GOAL := help

# Image coordinates. Override on the CLI:  make push IMAGE_REPO=myrepo/conduit
IMAGE_REPO ?= ghcr.io/$(shell git config user.name 2>/dev/null | tr '[:upper:]' '[:lower:]' | tr ' ' '-')/conduit
IMAGE_TAG  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

GO     := $(shell command -v go 2>/dev/null || echo $$HOME/go/bin/go)
GOTEST := $(GO) test -race -count=1

.PHONY: help dev dev-down test test-go test-web lint build \
        push push-server push-web \
        tf-init tf-plan deploy tf-destroy \
        clean

## ── Local dev ────────────────────────────────────────────────────────────────

help:           ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

dev:            ## Build images and start the full local stack (server + etcd + frontend)
	docker compose up --build

dev-down:       ## Tear down the local stack and remove volumes
	docker compose down -v

## ── Test & lint ──────────────────────────────────────────────────────────────

test: test-go test-web  ## Run all tests (Go + frontend build check)

test-go:        ## Run Go tests with the race detector
	export PATH="$$HOME/go/bin:$$PATH" && $(GOTEST) ./...

test-web:       ## Verify the frontend builds cleanly
	cd web && npm ci --prefer-offline && npm run build

lint:           ## Static analysis (go vet + go build check)
	export PATH="$$HOME/go/bin:$$PATH" && $(GO) vet ./...
	export PATH="$$HOME/go/bin:$$PATH" && $(GO) build ./...

## ── Docker ───────────────────────────────────────────────────────────────────

build:          ## Build both Docker images (server + frontend)
	docker build -t $(IMAGE_REPO):$(IMAGE_TAG)     -t $(IMAGE_REPO):latest     .
	docker build -t $(IMAGE_REPO)-web:$(IMAGE_TAG) -t $(IMAGE_REPO)-web:latest ./web

push: build     ## Push images to the registry
	docker push $(IMAGE_REPO):$(IMAGE_TAG)
	docker push $(IMAGE_REPO):latest
	docker push $(IMAGE_REPO)-web:$(IMAGE_TAG)
	docker push $(IMAGE_REPO)-web:latest

## ── Terraform ────────────────────────────────────────────────────────────────

tf-init:        ## Initialise Terraform (run once per workspace)
	cd deploy/terraform && terraform init

tf-plan:        ## Show Terraform plan without applying
	cd deploy/terraform && terraform plan -var="image_tag=$(IMAGE_TAG)"

deploy: push    ## Push images then apply Terraform changes
	cd deploy/terraform && terraform apply -auto-approve -var="image_tag=$(IMAGE_TAG)"

tf-destroy:     ## DANGER: destroy all cloud infrastructure
	cd deploy/terraform && terraform destroy -var="image_tag=$(IMAGE_TAG)"

## ── Misc ─────────────────────────────────────────────────────────────────────

clean:          ## Remove build artefacts
	rm -rf web/dist web/node_modules
	export PATH="$$HOME/go/bin:$$PATH" && $(GO) clean -cache
