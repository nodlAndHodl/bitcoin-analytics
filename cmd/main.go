package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/api"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/bitcoinrpc"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/marketdata"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Error loading .env file: %v", err)
	}

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-signalChan
		log.Printf("Received signal: %v. Shutting down...", sig)
		cancel()
	}()

	// Initialize database
	log.Println("Connecting to database...")
	dbConn, err := db.Connect()
	if err != nil {
		log.Fatalf("failed to connect to db: %v", err)
	}

	// Run migrations
	if err := db.MigrateModels(dbConn); err != nil {
		log.Fatalf("failed to migrate db: %v", err)
	}

	// Initialize Bitcoin RPC client
	log.Println("Connecting to Bitcoin RPC...")
	btcClient, err := bitcoinrpc.Connect()
	if err != nil {
		log.Fatalf("failed to connect to Bitcoin RPC: %v", err)
	}
	defer btcClient.Shutdown()

	// Start price data worker
	log.Println("Starting price data worker...")
	priceWorker := marketdata.NewWorker(
		dbConn,
		"btcusd", // Bitstamp currency pair
	)

	// Run the worker in a goroutine
	go priceWorker.Start()

	// Setup and start API server
	log.Println("Starting API server...")
	r := api.SetupRouter(btcClient, api.Config{})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Start server in a goroutine
	server := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("failed to run server: %v", err)
		}
	}()

	log.Printf("Server is running on port %s", port)

	// Wait for context cancellation (shutdown signal)
	<-ctx.Done()

	// Create a deadline to wait for server to shut down
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()

	// Attempt graceful shutdown
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}

	log.Println("Server stopped")
}
