# This file is part of kuberpult.

# Kuberpult is free software: you can redistribute it and/or modify
# it under the terms of the Expat(MIT) License as published by
# the Free Software Foundation.

# Kuberpult is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
# MIT License for more details.

# You should have received a copy of the MIT License
# along with kuberpult. If not, see <https://directory.fsf.org/wiki/License:Expat>.

# Copyright 2023 freiheit.com


include ../../Makefile.variables

MAKEFLAGS += --no-builtin-rules

export CGO_ENABLED=0

GO := go

GO_FILES := $(shell find . -type f -name '*.go')
PKG_GO_FILES := $(shell find ../../pkg/ -type f -name '*.go')
ALL_GO_FILES := $(GO_FILES) $(PKG_GO_FILES)


IMAGENAME?=$(IMAGE_REGISTRY)/kuberpult-frontend-service:${VERSION}

GARBAGE := *.sentinel

image-name:
	@echo "$(IMAGENAME)"

proto:
	$(MAKE) -C../../pkg/api all

bin/main: $(ALL_GO_FILES) | proto
	mkdir -p bin
	$(GO) build -o bin/main ./cmd/server/
GARBAGE += bin/main

export KUBERPULT_CDSERVER?=localhost:8443
export KUBERPULT_HTTP_CD_SERVER?=http://localhost:8080
export KUBERPULT_ALLOWED_ORIGINS?=localhost:*
export KUBERPULT_GIT_AUTHOR_NAME?=user-local-dev
export KUBERPULT_GIT_AUTHOR_EMAIL?=user-local-dev@example.com
export KUBERPULT_ARGOCD_BASE_URL=https://cd.dev.freiheit.systems/
export KUBERPULT_SOURCE_REPO_URL=https://github.com/freiheit-com/kuberpult/commit/{commit}
export KUBERPULT_MANIFEST_REPO_URL=https://github.com/freiheit-com/fdc-standard-setup-dev-env-manifest/tree/{branch}/{dir}
export KUBERPULT_GIT_BRANCH=main
run: bin/main
	./bin/main

build: bin/main

build-pr: test docker publish
	echo "build on pull request"

build-main: test docker publish
	echo "build on main"

.PHONY: cleanup-pr
cleanup-pr:
	echo "Nothing to clean"

.PHONY: cleanup-main
cleanup-main:
	echo "Nothing to clean"

.PHONY: publish
publish: release

start: src/api/api.ts
	pnpm i && pnpm start

clean:
	git clean -Xfd

.PHONY: test-dependencies
test-dependencies:
	make -C ../../pkg test
	make -C ../../pkg/api test

test: test-go lint-scss test-ts test-dependencies

test-go: $(ALL_GO_FILES) | proto
	$(GO) test ./...

test-ts: src/api/api.ts
	pnpm eslint
	# The extra '--' was added because of pnpm, Usage: pnpm test [-- <args>...]
	pnpm test -- --watchAll=false

lint-scss: deps
	pnpm lint-scss

docker: bin/main | dist
	docker build -t $(IMAGENAME) .

release: docker
	docker push $(IMAGENAME)

all: test docker

.PHONY: all release test docker clean

## Node part
NODE_BIN_DIR=$(shell pnpm bin)

deps: .deps.sentinel

.deps.sentinel: package.json
	pnpm i && touch .deps.sentinel

src/api/api.ts: ../../pkg/api/api.proto | deps
	mkdir -p src/api
	buf generate --path api --timeout 5m

dist: .dist.sentinel

.dist.sentinel: src/api/api.ts $(shell find public src -type f)
	rm -rf build/*
	pnpm build && touch .dist.sentinel


.DEFAULT_GOAL := all

.PHONY: deps

.PHONY: get-builder-image
get-builder-image:
	@echo "$(KUBERPULT_BUILDER)"
