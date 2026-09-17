GO := go

BIN := bin/server

# Платформа локальной Docker-сборки: Docker на Apple Silicon по умолчанию
# собирает linux/arm64, поэтому локальный бинарь собираем под неё же.
DOCKER_GOOS   ?= linux
DOCKER_GOARCH ?= $(shell go env GOARCH)

# Legacy-хост (Beget-VPS, `159.194.252.9`, hostname `ebszsvlknf`) — контур выведен из эксплуатации:
# сервер признан недоверенным (см. DEPLOY.md). Целевой контур — домашний сервер, задача deploy-home.
VPS_USER ?= root
VPS_HOST ?= ebszsvlknf
VPS_DIR  ?= ~/SportEvents

# Домашний сервер (доступен из домашней сети; SSH-алиас из ~/.ssh/config).
HOME_HOST ?= home-server
HOME_DIR  ?= /opt/projects/sportevents

COMPOSE := docker compose -f docker-compose.yml -f docker-compose.prod.yml

.PHONY: build vps-build test vet fmt deploy deploy-home

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

## deploy — LEGACY: собрать amd64-бинарь локально, скопировать на VPS и поднять там
## тонкий образ без компиляции Go на сервере.
deploy: vps-build
	ssh $(VPS_USER)@$(VPS_HOST) "mkdir -p $(VPS_DIR)/bin"
	scp $(BIN) $(VPS_USER)@$(VPS_HOST):$(VPS_DIR)/bin/server
	ssh $(VPS_USER)@$(VPS_HOST) "cd $(VPS_DIR) && git pull --ff-only && $(COMPOSE) up -d --build"

## deploy-home — обновить контур на домашнем сервере: сервер сам тянет код из git,
## собирает бинарь в контейнере golang и поднимает compose (/opt/projects/sportevents/deploy.sh).
deploy-home:
	ssh $(HOME_HOST) "bash $(HOME_DIR)/deploy.sh"
	ssh $(HOME_HOST) "bash $(HOME_DIR)/smoke.sh"
