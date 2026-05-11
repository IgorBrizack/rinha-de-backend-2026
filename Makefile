COMPOSE_DEV = docker compose -f docker-compose.dev.yaml
COMPOSE_PROD = docker compose

# ─── Desenvolvimento ──────────────────────────────────────────────────────────

.PHONY: dev
dev: ## Sobe o ambiente de desenvolvimento com hot reload
	$(COMPOSE_DEV) up -d --build

.PHONY: dev-debug
dev-debug: ## Sobe o ambiente de desenvolvimento sem hot reload (pronto para Delve)
	AIR_CONFIG=air.debug.toml $(COMPOSE_DEV) up -d --build

.PHONY: down
down: ## Derruba o ambiente de desenvolvimento
	$(COMPOSE_DEV) down

.PHONY: down-v
down-v: ## Derruba o ambiente de desenvolvimento e remove volumes
	$(COMPOSE_DEV) down -v

.PHONY: logs
logs: ## Exibe os logs do ambiente de desenvolvimento
	$(COMPOSE_DEV) logs -f

.PHONY: rebuild
rebuild: ## Reconstrói as imagens e sobe o ambiente de desenvolvimento
	$(COMPOSE_DEV) up --build --force-recreate

# ─── Produção ─────────────────────────────────────────────────────────────────

.PHONY: prod
prod: ## Sobe o ambiente de produção
	$(COMPOSE_PROD) up --build -d

.PHONY: prod-down
prod-down: ## Derruba o ambiente de produção
	$(COMPOSE_PROD) down

# ─── Build local ──────────────────────────────────────────────────────────────

.PHONY: build
build: ## Compila o binário localmente
	go build -o ./tmp/server ./cmd/server

.PHONY: run
run: build ## Compila e executa localmente (PORT=8080)
	PORT=8080 ./tmp/server

# ─── Qualidade ────────────────────────────────────────────────────────────────

.PHONY: test
test: ## Executa os testes unitários Go
	go test ./...

.PHONY: vet
vet: ## Executa go vet
	go vet ./...

.PHONY: k6
k6: ## Executa o cenário K6 contra BASE_URL (padrão: http://localhost:9999)
	k6 run $(if $(BASE_URL),--env BASE_URL=$(BASE_URL),) test/k6/scenario.js

.PHONY: k6-prod
k6-prod: ## Executa K6 contra o ambiente de produção local (docker compose up primeiro)
	BASE_URL=http://localhost:9999 $(MAKE) k6

# ─── Ajuda ────────────────────────────────────────────────────────────────────

.PHONY: help
help: ## Exibe esta mensagem de ajuda
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
