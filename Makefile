GO := go

BIN := bin/server

# Платформа локальной Docker-сборки: Docker на Apple Silicon по умолчанию
# собирает linux/arm64, поэтому локальный бинарь собираем под неё же.
DOCKER_GOOS   ?= linux
DOCKER_GOARCH ?= $(shell go env GOARCH)

# VPS-хост (задача deploy). Переопределяются из командной строки:
#   make deploy VPS_HOST=159.194.252.9 VPS_USER=root VPS_DIR=~/SportEvents
VPS_USER ?= root
VPS_HOST ?= ebszsvlknf
VPS_DIR  ?= ~/SportEvents

COMPOSE := docker compose -f docker-compose.yml -f docker-compose.prod.yml

.PHONY: build vps-build test vet fmt deploy

## build — собрать bin/server под локальную Docker-платформу (для docker compose up --build).
build:
	CGO_ENABLED=0 GOOS=$(DOCKER_GOOS) GOARCH=$(DOCKER_GOARCH) $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/server

## vps-build — кросс-компиляция под linux/amd64 (архитектура VPS).
vps-build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/server

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

fmt:
	gofmt -l .

## deploy — собрать amd64-бинарь локально, скопировать на VPS и поднять там
## тонкий образ без компиляции Go на сервере.
deploy: vps-build
	ssh $(VPS_USER)@$(VPS_HOST) "mkdir -p $(VPS_DIR)/bin"
	scp $(BIN) $(VPS_USER)@$(VPS_HOST):$(VPS_DIR)/bin/server
	ssh $(VPS_USER)@$(VPS_HOST) "cd $(VPS_DIR) && git pull --ff-only && $(COMPOSE) up -d --build"