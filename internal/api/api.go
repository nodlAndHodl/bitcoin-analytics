package api

import (
	"fmt"
	"log"
	"net/http"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/gin-gonic/gin"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/bitcoinrpc"
)

type Config struct {
	// Configuration for Bitcoin RPC client
}

type AddressBalanceResponse struct {
	Address string  `json:"address"`
	Balance float64 `json:"balance_btc"`
}

type UTXOResponse struct {
	TxID          string  `json:"txid"`
	Vout          uint32  `json:"vout"`
	Address       string  `json:"address"`
	Amount        float64 `json:"amount_btc"`
	Confirmations int64   `json:"confirmations"`
	Spendable     bool    `json:"spendable"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func handleGetAddressBalance(btcClient *rpcclient.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		address := c.Param("address")
		if address == "" {
			c.JSON(http.StatusBadRequest, ErrorResponse{"Address parameter is required"})
			return
		}

		log.Printf("Getting balance for address: %s", address)

		// Use the bitcoinrpc package's GetAddressBalance function
		balance, err := bitcoinrpc.GetAddressBalance(btcClient, address)
		if err != nil {
			log.Printf("Error getting balance for address %s: %v", address, err)
			c.JSON(http.StatusInternalServerError, ErrorResponse{err.Error()})
			return
		}

		log.Printf("Balance for address %s: %f BTC", address, balance)

		c.JSON(http.StatusOK, AddressBalanceResponse{
			Address: address,
			Balance: balance,
		})
	}
}

func handleGetAddressUTXOs(btcClient *rpcclient.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		address := c.Param("address")
		if address == "" {
			c.JSON(http.StatusBadRequest, ErrorResponse{"Address parameter is required"})
			return
		}

		log.Printf("Getting UTXOs for address: %s", address)
		
		// Use the bitcoinrpc package's GetAddressUTXOs function
		utxos, err := bitcoinrpc.GetAddressUTXOs(btcClient, address)
		if err != nil {
			log.Printf("Error getting UTXOs for address %s: %v", address, err)
			c.JSON(http.StatusInternalServerError, ErrorResponse{err.Error()})
			return
		}

		log.Printf("Found %d UTXOs for address %s", len(utxos), address)

		// Convert to our response format
		var response []UTXOResponse
		for _, utxo := range utxos {
			response = append(response, UTXOResponse{
				TxID:          utxo.TxID,
				Vout:          utxo.Vout,
				Address:       address,
				Amount:        utxo.Amount,
				Confirmations: utxo.Confirmations,
				Spendable:     utxo.Spendable,
			})
		}

		c.JSON(http.StatusOK, response)
	}
}

func getAddressUTXOs(client *rpcclient.Client, address string) ([]UTXOResponse, error) {
	// First, validate the address
	addr, err := btcutil.DecodeAddress(address, &chaincfg.MainNetParams)
	if err != nil {
		return nil, fmt.Errorf("invalid Bitcoin address: %v", err)
	}

	// Get all unspent transaction outputs for the address
	unspentOutputs, err := client.ListUnspentMinMaxAddresses(0, 9999999, []btcutil.Address{addr})
	if err != nil {
		return nil, fmt.Errorf("error getting unspent outputs: %v", err)
	}

	// Convert to our response format
	var result []UTXOResponse
	for _, utxo := range unspentOutputs {
		result = append(result, UTXOResponse{
			TxID:          utxo.TxID,
			Vout:          utxo.Vout,
			Address:       address,
			Amount:        utxo.Amount,
			Confirmations: utxo.Confirmations,
			Spendable:     utxo.Spendable,
		})
	}

	return result, nil
}

func SetupRouter(btcClient *rpcclient.Client, cfg Config) *gin.Engine {
	r := gin.Default()

	// Debug: Print all registered routes
	gin.DebugPrintRouteFunc = func(httpMethod, absolutePath, handlerName string, nuHandlers int) {
		log.Printf("ROUTE: %-6s %-40s --> %s (%d handlers)\n", httpMethod, absolutePath, handlerName, nuHandlers)
	}

	// Log configuration
	log.Printf("API Configuration: %+v", cfg)

	// Address routes
	addressGroup := r.Group("/address")
	{
		addressGroup.GET("/:address/balance", handleGetAddressBalance(btcClient))
		addressGroup.GET("/:address/utxos", handleGetAddressUTXOs(btcClient))
	}

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.GET("/block/height", func(c *gin.Context) {
		height, err := btcClient.GetBlockCount()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"block_height": height})
	})

	r.GET("/fee-estimate", func(c *gin.Context) {
		targets := []int64{1, 2, 3}
		estimates := make([]gin.H, 0, len(targets))
		for _, blocks := range targets {
			fee, err := btcClient.EstimateSmartFee(blocks, nil)
			if err != nil {
				estimates = append(estimates, gin.H{"blocks": blocks, "error": err.Error()})
				continue
			}
			estimate := gin.H{
				"blocks":              blocks,
				"feerate_btc_per_kvb": fee.FeeRate,
			}
			estimates = append(estimates, estimate)
		}
		c.JSON(http.StatusOK, gin.H{"estimates": estimates})
	})

	return r
}
