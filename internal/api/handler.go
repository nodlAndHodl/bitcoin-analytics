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
	"gorm.io/gorm"
)

type APIHandler struct {
	btcClient *rpcclient.Client
	config    Config
	db        *gorm.DB
}

func NewAPIHandler(btcClient *rpcclient.Client, config Config, db *gorm.DB) *APIHandler {
	return &APIHandler{
		btcClient: btcClient,
		config:    config,
		db:        db,
	}
}

func (h *APIHandler) SetupRoutes(router *gin.Engine) {
	// Health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// V1 API routes
	v1 := router.Group("/api/v1")
	{
		// Address endpoints
		v1.GET("/address/:address/balance", h.handleGetAddressBalance)
		v1.GET("/address/:address/utxos", h.handleGetAddressUTXOs)

		// Block endpoints
		v1.GET("/block/height", h.handleGetBlockHeight)
		v1.GET("/fee-estimate", h.handleGetFeeEstimate)
	}
}

// handleGetAddressBalance returns the balance for a given address
func (h *APIHandler) handleGetAddressBalance(c *gin.Context) {
	address := c.Param("address")
	if address == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{"Address parameter is required"})
		return
	}

	log.Printf("Getting balance for address: %s", address)

	// Use the bitcoinrpc package's GetAddressBalance function
	balance, err := bitcoinrpc.GetAddressBalance(h.btcClient, address)
	if err != nil {
		log.Printf("Error getting balance for address %s: %v", address, err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{err.Error()})
		return
	}

	c.JSON(http.StatusOK, AddressBalanceResponse{
		Address: address,
		Balance: balance,
	})
}

// handleGetAddressUTXOs returns the UTXOs for a given address
func (h *APIHandler) handleGetAddressUTXOs(c *gin.Context) {
	address := c.Param("address")
	if address == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{"Address parameter is required"})
		return
	}

	utxos, err := h.getAddressUTXOs(h.btcClient, address)
	if err != nil {
		log.Printf("Error getting UTXOs for address %s: %v", address, err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{err.Error()})
		return
	}

	c.JSON(http.StatusOK, utxos)
}

// handleGetBlockHeight returns the current block height
func (h *APIHandler) handleGetBlockHeight(c *gin.Context) {
	height, err := h.btcClient.GetBlockCount()
	if err != nil {
		log.Printf("Error getting block height: %v", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"height": height})
}

// handleGetFeeEstimate returns fee estimates for different confirmation targets
func (h *APIHandler) handleGetFeeEstimate(c *gin.Context) {
	// Define confirmation targets (in blocks)
	targets := []int{1, 2, 3, 4, 5}
	feeEstimates := make(map[int]float64)

	for _, target := range targets {
		feeResult, err := h.btcClient.EstimateSmartFee(int64(target), nil)
		if err != nil {
			log.Printf("Error estimating fee for target %d: %v", target, err)
			continue
		}

		if feeResult != nil && feeResult.FeeRate != nil && *feeResult.FeeRate > 0 {
			// Convert from BTC/kB to sat/vB
			feeEstimates[target] = *feeResult.FeeRate * 1e5 // 1 BTC/kB = 1e5 sat/vB (assuming 1 vB = 4 WU)
		}
	}

	c.JSON(http.StatusOK, feeEstimates)
}

// getAddressUTXOs returns the UTXOs for a given address
func (h *APIHandler) getAddressUTXOs(client *rpcclient.Client, address string) ([]UTXOResponse, error) {
	// Decode the address to get the script
	addr, err := btcutil.DecodeAddress(address, &chaincfg.MainNetParams)
	if err != nil {
		return nil, fmt.Errorf("invalid address: %v", err)
	}

	// Get the script for the address
	// Get the UTXOs for the address
	utxos, err := client.ListUnspentMinMaxAddresses(1, 9999999, []btcutil.Address{addr})
	if err != nil {
		return nil, fmt.Errorf("error getting UTXOs: %v", err)
	}

	// Convert to our response format
	result := make([]UTXOResponse, 0, len(utxos))
	for _, utxo := range utxos {
		// Skip unconfirmed transactions
		if utxo.Confirmations < 1 {
			continue
		}

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
