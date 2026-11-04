# Go and PHP are not installed on the host: everything runs in containers.
PORT ?= 8094
UID := $(shell id -u)
GID := $(shell id -g)
GO_RUN = docker run --rm -u $(UID):$(GID) -e GOCACHE=/tmp/gc -e GOMODCACHE=/tmp/gm -e HOME=/tmp \
	-v poc-vtex-gomod:/tmp/gm -v $(CURDIR)/.cache/go-build:/tmp/gc -v $(CURDIR):/src -w /src
GO_IMAGE = golang:1.26
PHP_IMAGE = php:8.3-cli-alpine

.PHONY: test test-go test-php test-integration up down demo

test: test-go test-php

.cache/go-build:
	mkdir -p $@

test-go: .cache/go-build
	$(GO_RUN) $(GO_IMAGE) go test -race -cover ./...

test-php:
	docker run --rm -v $(CURDIR)/mocks/sap-adapter-php:/app -w /app $(PHP_IMAGE) sh tests/run.sh

# Runs against the Redis of the compose stack, on its network.
test-integration: .cache/go-build
	docker compose up -d --wait redis
	$(GO_RUN) --network poc-vtex_backend -e REDIS_ADDR=redis:6379 $(GO_IMAGE) go test -tags integration -count=1 -v -run RealRedis ./internal/adapters/queue

up:
	PORT=$(PORT) docker compose up -d --build --wait

down:
	docker compose down -v

# Brings the stack up and runs the four scenarios against it.
demo: up .cache/go-build
	$(GO_RUN) --network host $(GO_IMAGE) go run ./cmd/demo -base http://127.0.0.1:$(PORT)
