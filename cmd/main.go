package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/nodlAndHodl/bitcoin-analytics/internal/analytics"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/api"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/bitcoinrpc"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/blockimporter"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/marketdata"
)

func main() {
	// Define command-line flags
	reprocessBlock := flag.Int64("reprocess", 0, "Specify a block height to reprocess. If > 0, the server will not start.")
	flag.Parse()

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

	// Handle reprocessing if the flag is set
	if *reprocessBlock > 0 {
		log.Printf("--- Reprocessing Block %d ---", *reprocessBlock)
		if err := blockImporter.ReprocessBlock(*reprocessBlock); err != nil {
			log.Fatalf("Failed to reprocess block %d: %v", *reprocessBlock, err)
		}
		log.Printf("--- Successfully reprocessed block %d ---", *reprocessBlock)
		return // Exit after reprocessing is complete
	}

	// Start block import in a separate goroutine
	go func() {
		if err := blockImporter.Start(); err != nil {
			log.Printf("Error starting block importer: %v", err)
		}
	}()
	defer blockImporter.Stop()

	// Start price data worker
	log.Println("Starting price data worker...")
	priceWorker := marketdata.PriceWorker(dbConn)
	go priceWorker.Start()

	// Start the analytics service and view refresher
	analyticsService := analytics.NewService(dbConn)
	// Refresh views every 6 hours. Adjust the interval as needed.
	go analyticsService.StartRefresher(6 * time.Hour)

	// Initialize API router using helper
	router := api.SetupRouter(btcClient, api.Config{})

	// Register explorer routes
	explorerHandler := api.NewExplorerHandler(dbConn)
	explorerHandler.RegisterRoutes(router)

	// Register analytics routes
	analyticsHandler := api.NewAnalyticsHandler(analyticsService)
	analyticsHandler.RegisterRoutes(router)

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
