package bitcoinrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
)

func Connect() (*rpcclient.Client, error) {
	host := os.Getenv("BTC_RPC_HOST")
	port := os.Getenv("BTC_RPC_PORT")
	user := os.Getenv("BTC_RPC_USER")
	pass := os.Getenv("BTC_RPC_PASS")

	// Remove http:// or https:// from host if present
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimPrefix(host, "https://")

	fullHost := fmt.Sprintf("%s:%s", host, port)

	// Force disable TLS for now to fix the HTTPS error
	disableTLS := true

	// Log connection details for debugging
	fmt.Printf("DEBUG: Connecting to Bitcoin RPC at %s (TLS disabled: %v)\n", fullHost, disableTLS)

	connCfg := &rpcclient.ConnConfig{
		Host:         fullHost,
		User:         user,
		Pass:         pass,
		HTTPPostMode: true,
		DisableTLS:   disableTLS,
	}

	// If host ends with .onion, use SOCKS5 proxy for Tor
	if strings.Contains(fullHost, ".onion") {
		proxy := os.Getenv("SOCKS5_PROXY")
		if proxy == "" {
			return nil, errors.New("SOCKS5_PROXY must be set in environment for .onion RPC host")
		}
		if strings.Contains(proxy, "socks5://") {
			connCfg.Proxy = proxy
		} else {
			socksProxy := "socks5://" + proxy
			connCfg.Proxy = socksProxy
		}
	}

	return rpcclient.New(connCfg, nil)
}

// GetAddressBalance returns the total balance for a given Bitcoin address in BTC
func GetAddressBalance(client *rpcclient.Client, address string) (float64, error) {
	// First, validate the address
	addr, err := btcutil.DecodeAddress(address, &chaincfg.MainNetParams)
	if err != nil {
		return 0, fmt.Errorf("invalid Bitcoin address: %v", err)
	}

	// Get all unspent transaction outputs for the address
	unspentOutputs, err := client.ListUnspentMinMaxAddresses(0, 9999999, []btcutil.Address{addr})
	if err != nil {
		return 0, fmt.Errorf("error getting unspent outputs: %v", err)
	}

	// Calculate total balance
	var totalBalance float64
	for _, output := range unspentOutputs {
		totalBalance += output.Amount
	}

	return totalBalance, nil
}

// ScanTxOutSetResult represents the result of a scantxoutset RPC call
type ScanTxOutSetResult struct {
	Success   bool   `json:"success"`
	TxOuts    int    `json:"txouts"`
	Height    int64  `json:"height"`
	BestBlock string `json:estblock"`
	UTXOs     []struct {
		TxID         string  `json:"txid"`
		Vout         uint32  `json:"vout"`
		ScriptPubKey string  `json:"scriptPubKey"`
		Desc         string  `json:"desc,omitempty"`
		Amount       float64 `json:"amount"`
		Height       int64   `json:"height"`
	} `json:"unspents"`
	TotalAmount float64 `json:"total_amount"`
}

// ScanTxOutSet scans the UTXO set for the given addresses
func ScanTxOutSet(client *rpcclient.Client, addresses []string) (*ScanTxOutSetResult, error) {
	if len(addresses) == 0 {
		return nil, fmt.Errorf("at least one address is required")
	}

	// Prepare the scan objects
	scanObjects := make([]string, 0, len(addresses))
	for _, addrStr := range addresses {
		_, err := btcutil.DecodeAddress(addrStr, &chaincfg.MainNetParams)
		if err != nil {
			return nil, fmt.Errorf("invalid address %s: %v", addrStr, err)
		}
		scanObjects = append(scanObjects, fmt.Sprintf("addr(%s)", addrStr))
	}

	// Log the scan request for debugging
	fmt.Printf("DEBUG: Scanning UTXOs for addresses: %v\n", addresses)

	// Try with different descriptor formats
	descriptorFormats := []string{
		// Try with addr() descriptor
		fmt.Sprintf(`["%s"]`, strings.Join(scanObjects, `","`)),
		// Try with raw address
		fmt.Sprintf(`["%s"]`, strings.Join(addresses, `","`)),
	}

	var lastErr error

	for _, desc := range descriptorFormats {
		// Prepare the raw request
		req := []json.RawMessage{
			json.RawMessage(`"start"`),
			json.RawMessage(desc),
		}

		// Log the request being sent
		fmt.Printf("DEBUG: Sending scantxoutset request with descriptor: %s\n", desc)

		// Call scantxoutset using RawRequest
		rawResp, err := client.RawRequest("scantxoutset", req)
		if err != nil {
			lastErr = fmt.Errorf("error scanning UTXO set with descriptor %s: %v", desc, err)
			fmt.Printf("DEBUG: Scan failed: %v\n", lastErr)
			continue
		}

		// Parse the raw response
		var result struct {
			Success   bool   `json:"success"`
			TxOuts    int    `json:"txouts"`
			Height    int64  `json:"height"`
			BestBlock string `json:"bestblock"`
			Unspents  []struct {
				TxID         string  `json:"txid"`
				Vout         uint32  `json:"vout"`
				ScriptPubKey string  `json:"scriptPubKey"`
				Desc         string  `json:"desc,omitempty"`
				Amount       float64 `json:"amount"`
				Height       int64   `json:"height"`
			} `json:"unspents"`
			TotalAmount float64 `json:"total_amount"`
		}

		if err := json.Unmarshal(rawResp, &result); err != nil {
			lastErr = fmt.Errorf("error parsing UTXO set response: %v", err)
			fmt.Printf("DEBUG: Failed to parse response: %v\n", lastErr)
			continue
		}

		// Log the raw response for debugging
		fmt.Printf("DEBUG: Raw response: %s\n", string(rawResp))

		fmt.Printf("DEBUG: Success: %v, Found %d UTXOs, Total amount: %f\n",
			result.Success, len(result.Unspents), result.TotalAmount)

		// Convert the result to our struct
		var scanResult ScanTxOutSetResult
		scanResult.Success = result.Success
		scanResult.TxOuts = result.TxOuts
		scanResult.Height = result.Height
		scanResult.BestBlock = result.BestBlock
		scanResult.TotalAmount = result.TotalAmount

		// Convert UTXOs
		for _, utxo := range result.Unspents {
			scanResult.UTXOs = append(scanResult.UTXOs, struct {
				TxID         string  `json:"txid"`
				Vout         uint32  `json:"vout"`
				ScriptPubKey string  `json:"scriptPubKey"`
				Desc         string  `json:"desc,omitempty"`
				Amount       float64 `json:"amount"`
				Height       int64   `json:"height"`
			}{
				TxID:         utxo.TxID,
				Vout:         utxo.Vout,
				ScriptPubKey: utxo.ScriptPubKey,
				Desc:         utxo.Desc,
				Amount:       utxo.Amount,
				Height:       utxo.Height,
			})
		}

		// If we got here, we have a successful scan
		return &scanResult, nil
	}

	// If we get here, all descriptor formats failed
	return nil, fmt.Errorf("all scan attempts failed. Last error: %v", lastErr)
}

// GetAddressUTXOs gets UTXOs for a single address using scantxoutset
func GetAddressUTXOs(client *rpcclient.Client, address string) ([]btcjson.ListUnspentResult, error) {
	fmt.Printf("DEBUG: Getting UTXOs for address: %s\n", address)

	// First, validate the address
	_, err := btcutil.DecodeAddress(address, &chaincfg.MainNetParams)
	if err != nil {
		return nil, fmt.Errorf("invalid Bitcoin address: %v", err)
	}

	// Try using scantxoutset first
	fmt.Printf("DEBUG: Attempting to scan UTXOs using scantxoutset\n")
	result, err := ScanTxOutSet(client, []string{address})
	if err != nil {
		fmt.Printf("DEBUG: scantxoutset failed: %v\n", err)
	} else {
		// Convert the result to ListUnspentResult format
		var unspentOutputs []btcjson.ListUnspentResult
		for _, utxo := range result.UTXOs {
			_, err := chainhash.NewHashFromStr(utxo.TxID)
			if err != nil {
				fmt.Printf("DEBUG: Invalid TXID %s: %v\n", utxo.TxID, err)
				continue
			}

			confirmations := int64(0)
			if result.Height > 0 && utxo.Height > 0 {
				confirmations = result.Height - utxo.Height + 1
			}

			unspentOutputs = append(unspentOutputs, btcjson.ListUnspentResult{
				TxID:          utxo.TxID,
				Vout:          utxo.Vout,
				Address:       address,
				ScriptPubKey:  utxo.ScriptPubKey,
				Amount:        utxo.Amount,
				Confirmations: confirmations,
			})
		}

		if len(unspentOutputs) > 0 {
			fmt.Printf("DEBUG: Found %d UTXOs using scantxoutset\n", len(unspentOutputs))
			return unspentOutputs, nil
		}
		fmt.Println("DEBUG: No UTXOs found using scantxoutset")
	}

	// Fall back to the wallet-based method if scantxoutset fails or returns no results
	fmt.Println("DEBUG: Falling back to wallet-based method")
	addr, err := btcutil.DecodeAddress(address, &chaincfg.MainNetParams)
	if err != nil {
		return nil, fmt.Errorf("invalid Bitcoin address: %v", err)
	}

	// Get all unspent transaction outputs for the address
	unspentOutputs, err := client.ListUnspentMinMaxAddresses(0, 9999999, []btcutil.Address{addr})
	if err != nil {
		return nil, fmt.Errorf("error getting unspent outputs: %v", err)
	}

	fmt.Printf("DEBUG: Found %d UTXOs using wallet method\n", len(unspentOutputs))
	return unspentOutputs, nil
}
