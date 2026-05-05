# rinha-de-backend-2026

API de detecção de fraudes para a [Rinha de Backend 2026](https://github.com/zanfranceschi/rinha-de-backend-2026).

## Arquitetura

```
                        ┌─────────────────────────────┐
                        │         nginx :9999          │
                        │      (round-robin LB)        │
                        └────────────┬────────────────┘
                                     │
                  ┌──────────────────┴──────────────────┐
                  ▼                                      ▼
         ┌────────────────┐                   ┌────────────────┐
         │   api1 :8080   │                   │   api2 :8080   │
         │   (Go/stdlib)  │                   │   (Go/stdlib)  │
         └────────────────┘                   └────────────────┘
```

- **Load balancer**: nginx com round-robin simples, sem lógica de negócio
- **API**: Go 1.24, stdlib pura, sem dependências externas
- **Limites de recursos**: 1 CPU / 350 MB total (nginx 0.1/50MB, cada instância 0.45/150MB)

### Estrutura interna (DDD)

```
internal/
  domain/       ← entidades e interface FraudScorer
  usecase/      ← ScoreFraud (orquestração)
  scorer/       ← implementação do scorer (NoOp placeholder)
  handler/      ← HTTP handlers e roteamento
```

## Endpoints

### `GET /ready`
Verificação de prontidão. Retorna `200 OK` quando a API está pronta.

### `POST /fraud-score`
Avalia o risco de fraude de uma transação.

**Request:**
```json
{
  "id": "tx-3576980410",
  "transaction": {
    "amount": 384.88,
    "installments": 3,
    "requested_at": "2026-03-11T20:23:35Z"
  },
  "customer": {
    "avg_amount": 769.76,
    "tx_count_24h": 3,
    "known_merchants": ["MERC-009", "MERC-001"]
  },
  "merchant": {
    "id": "MERC-001",
    "mcc": "5912",
    "avg_amount": 298.95
  },
  "terminal": {
    "is_online": false,
    "card_present": true,
    "km_from_home": 13.7
  },
  "last_transaction": {
    "timestamp": "2026-03-11T14:58:35Z",
    "km_from_current": 18.86
  }
}
```

**Response:**
```json
{
  "approved": true,
  "fraud_score": 0.0
}
```

## Como executar

### Produção

```bash
docker compose up --build
```

Acesse em `http://localhost:9999`.

### Desenvolvimento (hot reload + debug)

```bash
docker compose -f docker-compose.dev.yaml up --build
```

O [Air](https://github.com/air-verse/air) monitora alterações nos arquivos `.go` e recompila automaticamente.

#### Debug remoto com VS Code

1. Suba o ambiente dev conforme acima
2. No VS Code, acesse **Run and Debug** (`Ctrl+Shift+D`)
3. Selecione **Delve into Docker** e pressione `F5`

O Delve se conecta ao container `rinha-api1` na porta `2345`.

## Desenvolvimento local (sem Docker)

```bash
go build ./cmd/server
PORT=8080 ./server
```
