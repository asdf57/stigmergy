BINARY := build/homelab-controller

.DEFAULT_GOAL := build

.PHONY: generate fmt test test-integration vet build run run-local up up-tools down logs clean

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
	docker compose up --build

run-local:
	go run ./cmd/homelab-controller

up:
	docker compose up --build --detach

up-tools:
	docker compose --profile tools up --build --detach

down:
	docker compose --profile tools down

logs:
	docker compose logs --follow

clean:
	rm -rf build
