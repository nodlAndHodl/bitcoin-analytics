package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/nodlAndHodl/bitcoin-analytics/internal/api"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/bitcoinrpc"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/blockimporter"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/marketdata"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/peercrawler"
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

	// Start peer crawler
	log.Println("Starting peer crawler...")

	peerCrawlerWorkersStr := os.Getenv("PEER_CRAWLER_WORKERS")
	peerCrawlerWorkers, err := strconv.Atoi(peerCrawlerWorkersStr)
	if err != nil || peerCrawlerWorkers <= 0 {
		log.Printf("Invalid or missing PEER_CRAWLER_WORKERS, defaulting to 3. Error: %v", err)
		peerCrawlerWorkers = 3
	}

	// DNS seeds and hard-coded reachable nodes
	dnsSeeds := []string{
		"seed.bitcoin.sipa.be",
		"dnsseed.bitcoin.dashjr.org",
		"seed.bitcoinstats.com",
		"seed.bitcoin.jonasschnelli.ch",
	}
	hardcoded := []string{
		// Clearnet nodes
		"47.96.110.251:8333",  // example China
		"152.89.162.238:8333", // example EU
		"38.242.220.211:8333", // example US

		// Tor nodes (v3 onion addresses)
		"ufmzgrvn2uxzclapi2qzbuqq2nmezjnimnvxxuddyojkzz6jvtnnmgid.onion:8333",
		"i2f3uxhpecs7nf3litesvf7m6bbupgd2ejmtxlsseparw6onmawdx7id.onion:8333",
		"cc7hh7dc3m4yl2vjui4vy43rspwqr7ss5btesd4kd5tpnopp2mho3ryd.onion:8333", // Bitcoin Core node
		"bk7yp6epnmcllq72pwb4j4z7ioo3vyfgwm4xcr4ge6n5mnbfd3kfyxad.onion:8333", // Umbrel node
	}
	var seedAddrs []string
	// resolve DNS seeds
	for _, host := range dnsSeeds {
		if ips, err := net.LookupHost(host); err == nil {
			for _, ip := range ips {
				seedAddrs = append(seedAddrs, net.JoinHostPort(ip, "8333"))
			}
		}
	}
	seedAddrs = append(seedAddrs, hardcoded...)
	// remove duplicates
	seenSeed := make(map[string]struct{})
	uniqueSeeds := make([]string, 0, len(seedAddrs))
	for _, a := range seedAddrs {
		key := strings.ToLower(a)
		if _, ok := seenSeed[key]; !ok {
			seenSeed[key] = struct{}{}
			uniqueSeeds = append(uniqueSeeds, a)
		}
	}

	// Combine any unique seeds with default Bitcoin seed nodes
	defaultSeeds := []string{
		"seed.bitcoin.sipa.be:8333",
		"dnsseed.bluematt.me:8333",
		"dnsseed.bitcoin.dashjr.org:8333",
		"seed.bitcoinstats.com:8333",
		"seed.bitcoin.jonasschnelli.ch:8333",
		"seed.btc.petertodd.org:8333",
	}

	// Add default seeds to uniqueSeeds
	for _, seed := range defaultSeeds {
		key := strings.ToLower(seed)
		if _, ok := seenSeed[key]; !ok {
			seenSeed[key] = struct{}{}
			uniqueSeeds = append(uniqueSeeds, seed)
		}
	}

	pc := peercrawler.NewCrawler(dbConn, uniqueSeeds)
	go pc.Start(ctx)

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
	priceWorker := marketdata.PriceWorker(
		dbConn,
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
