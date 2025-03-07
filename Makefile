# OpenShift Marketplace - Build and Test

SHELL := /bin/bash
PKG := github.com/operator-framework/operator-marketplace/pkg
MOCKS_DIR := ./pkg/mocks
CONTROLLER_RUNTIME_PKG := sigs.k8s.io/controller-runtime/pkg
OPERATORSOURCE_MOCK_PKG := operatorsource_mocks

# If the GOBIN environment variable is set, 'go install' will install the
# commands to the directory it names, otherwise it will default of $GOPATH/bin.
# GOBIN must be an absolute path.
ifeq ($(GOBIN),)
mockgen := $(GOPATH)/bin/mockgen
else
mockgen := $(GOBIN)/mockgen
endif

all: build

build: osbs-build

osbs-build:
	./build/build.sh

unit: unit-test

unit-test:
	go test -v ./pkg/...

e2e: e2e-job

e2e-job:
	go test -v -race -failfast -timeout 90m ./test/e2e/... --ginkgo.randomizeAllSpecs

install-olm-crds:
	kubectl apply -f https://github.com/operator-framework/operator-lifecycle-manager/releases/download/v0.17.0/crds.yaml

.PHONY: vendor
vendor:
	go mod tidy
	go mod vendor
	go mod verify

.PHONY: manifests
manifests: generate
	./hack/update-manifests.sh

generate:
	./hack/openapi-gen.sh
