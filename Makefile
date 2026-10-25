BINARY   := ccdav
PKG      := ./cmd/ccdav
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

ENGINE   ?= podman
REGISTRY ?=
# Full image name; REGISTRY is a prefix: make image REGISTRY=registry.example.com/me TAG=231
IMAGE    ?= $(if $(REGISTRY),$(patsubst %/,%,$(REGISTRY))/ccdav,ccdav)
TAG      ?= $(VERSION)
DATA     ?= $(CURDIR)/data
PORT     ?= 8080
FUZZTIME ?= 30s

GO       ?= go
export CGO_ENABLED := 0

.PHONY: all build test race vet vulncheck fmt fmt-check check fuzz run clean \
        image image-push image-release image-login image-run hash-password help need-registry

all: check build

help: ## show targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

build: ## build bin/ccdav
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

test: ## run unit tests
	$(GO) test ./...

race: ## run tests with the race detector (needs cgo)
	CGO_ENABLED=1 $(GO) test -race ./...

vet: ## go vet
	$(GO) vet ./...

vulncheck: ## known vulnerabilities in dependencies and stdlib (govulncheck)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

fmt: ## gofmt in place
	gofmt -w -s .

fmt-check:
	@out=$$(gofmt -l -s .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: fmt-check vet test ## fmt-check + vet + test

fuzz: ## run every fuzz target for FUZZTIME each
	@for pkg in $$($(GO) list ./...); do \
	  for f in $$($(GO) test -list '^Fuzz' $$pkg | grep '^Fuzz'); do \
	    echo "== $$pkg $$f"; \
	    $(GO) test $$pkg -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZTIME) || exit 1; \
	  done; \
	done

run: build ## run locally against ./data
	@test -f $(DATA)/config.yaml || { mkdir -p $(DATA); cp config.example.yaml $(DATA)/config.yaml; \
	  echo "created $(DATA)/config.yaml from example (user alice / password alice)"; }
	./bin/$(BINARY) -data $(DATA) -listen :$(PORT)

hash-password: build ## hash a password for config.yaml
	@./bin/$(BINARY) hash-password

image: ## build the image, tagged TAG and latest (set REGISTRY=, IMAGE=, TAG=)
	$(ENGINE) build -f Containerfile --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

image-push: need-registry ## push IMAGE:TAG (build it first with make image)
	$(ENGINE) push $(IMAGE):$(TAG)

image-release: need-registry image ## build, push TAG and move latest along
	$(ENGINE) push $(IMAGE):$(TAG)
	$(ENGINE) push $(IMAGE):latest

image-login: need-registry ## log in to REGISTRY's host
	$(ENGINE) login $(firstword $(subst /, ,$(REGISTRY)))

image-run: image ## run the image with ./data mounted at /data
	@test -f $(DATA)/config.yaml || { mkdir -p $(DATA); cp config.example.yaml $(DATA)/config.yaml; }
	$(ENGINE) run --rm -it -p $(PORT):8080 --userns keep-id -v $(DATA):/data:Z $(IMAGE):$(TAG)

clean: ## remove build output
	rm -rf bin

need-registry:
	@test -n "$(REGISTRY)" || { echo "set REGISTRY=host/path, e.g. make image-push REGISTRY=registry.example.com/me"; exit 1; }
