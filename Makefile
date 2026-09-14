BINARY := build/homelab-controller
COMPOSE_DEV := docker compose -f compose.yaml -f compose.etcd.yaml -f compose.openbao.yaml

.DEFAULT_GOAL := build

.PHONY: generate fmt test test-integration vet build run run-api run-local up up-api up-tools down logs openbao-root-token clean

generate:
	go generate ./...

fmt:
	go fmt ./...

test:
	go test ./...

test-integration:
	ETCD_ENDPOINTS=$${ETCD_ENDPOINTS:-http://127.0.0.1:2379} go test -tags=integration ./internal/store/etcd

vet:
	go vet ./...

build:
	mkdir -p build
	go build -trimpath -o $(BINARY) ./cmd/homelab-controller

run:
	$(COMPOSE_DEV) up --build

run-api:
	docker compose up --build api

run-local:
	go run ./cmd/homelab-controller

up:
	$(COMPOSE_DEV) up --build --detach

up-api:
	docker compose up --build --detach api

up-tools:
	$(COMPOSE_DEV) --profile tools up --build --detach

down:
	$(COMPOSE_DEV) --profile tools down

logs:
	$(COMPOSE_DEV) logs --follow

openbao-root-token:
	$(COMPOSE_DEV) run --rm --no-deps --entrypoint /bin/sh openbao-bootstrap -c 'cat /run/openbao-init/root-token'

clean:
	rm -rf build
