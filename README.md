# Bitcoin Analytics API

A Go web API to aggregate and store Bitcoin address amounts per block, using Postgres and Bitcoin Core RPC.

## Features
- REST API (Gin)
- Postgres for storage
- Docker Compose for orchestration
- Connects to your own Bitcoin Core node via RPC

## Getting Started

1. Clone the repo.
2. Set your Bitcoin Core RPC credentials in `docker-compose.yml` or as environment variables.
3. Run:
   ```
   docker-compose up --build
   ```
4. The API will be available at [http://localhost:8080/health](http://localhost:8080/health)

## Environment Variables
- `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME`
- `BTC_RPC_HOST`, `BTC_RPC_PORT`, `BTC_RPC_USER`, `BTC_RPC_PASS`

## Next Steps
- Add endpoints for address aggregation and block scanning.
- Implement block iteration and database population logic.
