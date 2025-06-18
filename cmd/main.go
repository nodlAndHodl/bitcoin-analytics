package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

		"github.com/joho/godotenv"

	"github.com/nodlAndHodl/bitcoin-analytics/internal/api"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/bitcoinrpc"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/blockimporter"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/marketdata"
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

	// Initialize block importer
	blockImporter := blockimporter.NewBlockImporter(dbConn, btcClient)
	
	// Start block import in a separate goroutine
	go func() {
		if err := blockImporter.Start(); err != nil {
			log.Printf("Error starting block importer: %v", err)
		}
	}()
	defer blockImporter.Stop()

	// Start price data worker
	log.Println("Starting price data worker...")
	priceWorker := marketdata.NewWorker(
		dbConn,
		"btcusd", // Bitstamp currency pair
	)

	// Run the worker in a goroutine
	go priceWorker.Start()

	// Initialize API router using helper
	router := api.SetupRouter(btcClient, api.Config{})

	// Register explorer routes
	explorerHandler := api.NewExplorerHandler(dbConn)
	explorerHandler.RegisterRoutes(router)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Start server in a goroutine
	server := &http.Server{
		Addr:    fmt.Sprintf(":%s", port),
		Handler: router,
	}

	go func() {
		log.Printf("Server starting on port %s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
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
