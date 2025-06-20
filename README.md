# Bitcoin Analytics API

A Go web API to aggregate and store Bitcoin address amounts per block, using Postgres and Bitcoin Core RPC.

## Features
- REST API (Gin)
- Postgres for storage
- Docker Compose for orchestration
- Connects to your own Bitcoin Core node via RPC

## Getting Started

### Prerequisites
- Go (1.18+)
- Docker and Docker Compose
- A running Bitcoin Core node (local or remote)

### Configuration

1.  **Set up environment variables**:
    This project uses a `.env` file for configuration. Start by copying the example file:
    ```bash
    cp .env.example .env
    ```

2.  **Edit the `.env` file**:
    Open the newly created `.env` file and fill in the details for your Bitcoin Core RPC and PostgreSQL database connections.

### Running the Application

1.  **With Docker (Recommended)**:
    ```bash
    docker-compose up --build
    ```

2.  **Locally**:
    ```bash
    go run ./cmd/main.go
    ```

The API will be available at [http://localhost:8080/health](http://localhost:8080/health).

## Next Steps
- Add endpoints for address aggregation and block scanning.
- Implement block iteration and database population logic.



A function to calculate Circulating Supply at a given time/block height.
A function to calculate Market Value at a given time.
A function to calculate Realized Value at a given time.
A function to calculate the MVRV Z-Score itself, using the outputs of the above.