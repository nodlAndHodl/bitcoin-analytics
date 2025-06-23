package blockimporter

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
)

type BlockImporter struct {
	DB       *gorm.DB
	RPC      *rpcclient.Client
	shutdown chan struct{}
}

func NewBlockImporter(db *gorm.DB, rpc *rpcclient.Client) *BlockImporter {
	return &BlockImporter{
		DB:       db,
		RPC:      rpc,
		shutdown: make(chan struct{}),
	}
}

const pollInterval = 5 * time.Minute // how often to check for new blocks after initial sync

// Start begins the block import process. It performs an initial one-off catch-up to the
// node tip. After that completes it polls the node tip every `pollInterval` and imports
// any new blocks that have arrived. The call is blocking while the initial sync runs –
// run it in a goroutine if you do not want to block.
func (bi *BlockImporter) Start() error {
	// Get the current block height from the node
	nodeHeight, err := bi.RPC.GetBlockCount()
	if err != nil {
		return fmt.Errorf("failed to get block count: %v", err)
	}

	// Get the current height from the database
	var currentHeight int64
	result := bi.DB.Model(&db.Block{}).Select("COALESCE(MAX(height), -1)").Scan(&currentHeight)
	if result.Error != nil {
		return fmt.Errorf("failed to get current height: %v", result.Error)
	}

	// Start importing from the next block (if we are behind)
	startHeight := currentHeight + 1
	if startHeight <= nodeHeight {
		log.Printf("Starting block import from height %d to %d", startHeight, nodeHeight)
		bi.importBlocks(startHeight, nodeHeight) // blocking until catch-up complete
		// All addresses and UTXOs are now processed in the first pass
	} else {
		log.Printf("already at latest block height: %d", currentHeight)
	}

	// Begin periodic polling for new blocks once the initial catch-up is finished
	log.Printf("entering polling mode – will check for new blocks every %s", pollInterval)
	ticker := time.NewTicker(pollInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-bi.shutdown:
				return
			case <-ticker.C:
				// Determine current db tip
				var dbTip int64
				_ = bi.DB.Model(&db.Block{}).Select("COALESCE(MAX(height), -1)").Scan(&dbTip)

				nodeTip, err := bi.RPC.GetBlockCount()
				if err != nil {
					log.Printf("failed to get block count: %v", err)
					continue
				}

				if nodeTip > dbTip {
					log.Printf("detected new blocks – importing %d to %d", dbTip+1, nodeTip)
					bi.importBlocks(dbTip+1, nodeTip)
				}
			}
		}
	}()

	return nil
}

// Stop signals the importer to shut down
func (bi *BlockImporter) Stop() {
	close(bi.shutdown)
}

func (bi *BlockImporter) importBlocks(startHeight, endHeight int64) {
	for height := startHeight; height <= endHeight; height++ {
		select {
		case <-bi.shutdown:
			log.Println("Block import stopped by shutdown signal")
			return
		default:
			hash, err := bi.RPC.GetBlockHash(height)
			if err != nil {
				log.Printf("Error getting block hash at height %d: %v", height, err)
				continue
			}

			// Get block with verbose transaction data
			block, err := bi.RPC.GetBlockVerboseTx(hash)
			if err != nil {
				log.Printf("Error getting block at height %d: %v", height, err)
				continue
			}

			if err := bi.processBlock(block); err != nil {
				log.Printf("Error processing block %d: %v", height, err)
				continue
			}

			if height%1000 == 0 || height == endHeight {
				log.Printf("Processed block %d/%d (%.2f%%)", height, endHeight, float64(height)/float64(endHeight)*100)
			}
		}
	}
}

func (bi *BlockImporter) processBlock(block *btcjson.GetBlockVerboseTxResult) error {
	// Convert block time to time.Time
	blockTime := time.Unix(block.Time, 0)
	// For now, use block time as median time
	// The MedianTime field is not available in the verbose block response
	medianTime := blockTime

	// Create block record
	blockRecord := &db.Block{
		Height:            block.Height,
		Hash:              block.Hash,
		Version:           int32(block.Version),
		VersionHex:        fmt.Sprintf("%08x", block.Version),
		MerkleRoot:        block.MerkleRoot,
		Time:              blockTime,
		MedianTime:        medianTime,
		Nonce:             block.Nonce,
		Bits:              block.Bits,
		Difficulty:        block.Difficulty,
		NTx:               len(block.Tx),
		PreviousBlockHash: block.PreviousHash,
		NextBlockHash:     block.NextHash,
		StrippedSize:      int(block.StrippedSize),
		Size:              int(block.Size),
		Weight:            int(block.Weight),
	}

	// Store transaction hashes as JSON

	// Start a DB transaction for this block and its transactions
	dbTx := bi.DB.Begin()
	if dbTx.Error != nil {
		return fmt.Errorf("failed to begin db transaction: %v", dbTx.Error)
	}

	// Handle any panics and rollback
	defer func() {
		if r := recover(); r != nil {
			dbTx.Rollback()
			log.Printf("recovered from panic in processBlock: %v", r)
		}
	}()

	// Insert block
	if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(blockRecord).Error; err != nil {
		dbTx.Rollback()
		return fmt.Errorf("failed to insert block: %v", err)
	}

	// Process each transaction in the block
	// Use the transaction data already provided by GetBlockVerboseTx
	// This eliminates the need for additional RPC calls
	for _, txData := range block.Tx {
		// Process transaction - first pass only stores transaction data
		err := bi.processTransaction(dbTx, txData, block.Height)
		if err != nil {
			dbTx.Rollback()
			return fmt.Errorf("failed to process transaction: %v", err)
		}
	}

	// Commit the transaction
	if err := dbTx.Commit().Error; err != nil {
		return fmt.Errorf("failed to commit transaction: %v", err)
	}

	return nil
}

// Constants for batch processing
const (
	AddrTxBatchSize = 1000 // Increased batch size for better performance
	BlockBatchSize  = 100  // Process this many blocks before processing their addresses
	MaxHeight       = -1   // Used to process all available blocks
)

// First-pass processing: Store blocks and transaction data, and address transactions
func (bi *BlockImporter) processTransaction(dbTx *gorm.DB, txData btcjson.TxRawResult, blockHeight int64) error {
	// Track addresses involved in this transaction to avoid double-counting tx_count
	addressesInTx := make(map[string]bool)

	// Check if this is a coinbase transaction (first transaction in a block)
	isCoinbase := len(txData.Vin) > 0 && txData.Vin[0].Coinbase != ""
	// Create transaction record
	txRecord := &db.Transaction{
		BlockHeight: blockHeight,
		Hex:         txData.Hex,
		Txid:        txData.Txid,
		Hash:        txData.Hash,
		Size:        int(txData.Size),
		Vsize:       int(txData.Vsize),
		Weight:      int(txData.Weight),
		Version:     int32(txData.Version),
		Locktime:    uint32(txData.LockTime),
	}

	// Serialize vin to JSON
	vinJSON, err := json.Marshal(txData.Vin)
	if err != nil {
		return fmt.Errorf("failed to marshal vin: %v", err)
	}
	txRecord.Vin = vinJSON

	// Serialize vout to JSON
	voutJSON, err := json.Marshal(txData.Vout)
	if err != nil {
		return fmt.Errorf("failed to marshal vout: %v", err)
	}
	txRecord.Vout = voutJSON

	// Save transaction, ignore duplicates
	if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(txRecord).Error; err != nil {
		return fmt.Errorf("failed to save transaction: %v", err)

	}

	// Process inputs - create negative address transactions for spent outputs
	for _, input := range txData.Vin {
		// Skip coinbase inputs
		if isCoinbase {
			continue
		}

		// We need to find the referenced transaction output information
		// First check if we can find an existing address transaction for this output
		var referencedTx db.Transaction
		result := dbTx.Where("txid = ?", input.Txid).First(&referencedTx)

		if result.Error == nil {
			// Process existing transaction in our database
			var vouts []btcjson.Vout
			if err := json.Unmarshal(referencedTx.Vout, &vouts); err != nil {
				log.Printf("Error unmarshaling vout data for tx %s: %v", input.Txid, err)
				continue
			}

			// Make sure the vout index is within range
			if int(input.Vout) >= len(vouts) {
				log.Printf("Vout index %d out of range for tx %s", input.Vout, input.Txid)
				continue
			}

			// Get the output being spent
			output := vouts[input.Vout]

			// Extract the address(es) from the output
			var addresses []string
			if len(output.ScriptPubKey.Addresses) > 0 {
				addresses = output.ScriptPubKey.Addresses
			} else if output.ScriptPubKey.Hex != "" {
				// Parse the script to get addresses
				script, err := hex.DecodeString(output.ScriptPubKey.Hex)
				if err == nil {
					class, scriptAddrs, _, err := txscript.ExtractPkScriptAddrs(script, &chaincfg.MainNetParams)
					if err == nil {
						// Special case for P2PK scripts (mempool.space style)
						if class == txscript.PubKeyTy {
							// Keep raw pubkey as identifier
							if pushed, err := txscript.PushedData(script); err == nil && len(pushed) > 0 {
								pubkeyStr := hex.EncodeToString(pushed[0])
								addresses = append(addresses, pubkeyStr)
							}
						} else if len(scriptAddrs) > 0 {
							// Standard script with addresses
							for _, scriptAddr := range scriptAddrs {
								addresses = append(addresses, scriptAddr.EncodeAddress())
							}
						}
					}
				}
			}

			// Convert BTC to satoshis
			amtSat := int64(output.Value * 100000000)

			// Create negative address transaction records for each address
			for _, addr := range addresses {
				// Mark this address as seen in this transaction
				if _, seen := addressesInTx[addr]; !seen {
					addressesInTx[addr] = true
				}

				// Create negative address transaction record for the spend
				addrTx := db.AddressTransaction{
					ID:          uuid.New(),
					Address:     addr,
					TxID:        txData.Txid,
					BlockHeight: blockHeight,
					Amount:      -amtSat, // Negative for inputs/spends
					Coinbase:    false,   // Input spends can never be coinbase
					InputTxId:   &input.Txid,
					InputVout:   func() *int { v := int(input.Vout); return &v }(),
				}

				if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&addrTx).Error; err != nil {
					log.Printf("Error creating spend address transaction for %s: %v", addr, err)
				}
			}
		}
	}

	// Process outputs to create address_transactions
	for _, output := range txData.Vout {
		// Skip outputs with no value
		if output.Value <= 0 {
			continue
		}

		// Convert BTC to satoshis
		amtSat := int64(output.Value * 100000000)

		// Extract addresses from output
		var addresses []string

		// First check if RPC server provided addresses
		if len(output.ScriptPubKey.Addresses) > 0 {
			addresses = output.ScriptPubKey.Addresses
		} else if output.ScriptPubKey.Hex != "" {
			// If no addresses but we have script hex, parse it
			script, err := hex.DecodeString(output.ScriptPubKey.Hex)
			if err == nil {
				class, scriptAddrs, _, err := txscript.ExtractPkScriptAddrs(script, &chaincfg.MainNetParams)
				if err == nil {
					// Special case for P2PK scripts (mempool.space style)
					if class == txscript.PubKeyTy {
						// Keep raw pubkey as identifier
						if pushed, err := txscript.PushedData(script); err == nil && len(pushed) > 0 {
							pubkeyStr := hex.EncodeToString(pushed[0])
							addresses = append(addresses, pubkeyStr)
						}
					} else if len(scriptAddrs) > 0 {
						// Standard script with addresses
						for _, scriptAddr := range scriptAddrs {
							addresses = append(addresses, scriptAddr.EncodeAddress())
						}
					}
				}
			}
		}

		// If we found addresses, create the necessary records
		for _, addr := range addresses {
			// Mark address as seen in this transaction
			if _, seen := addressesInTx[addr]; !seen {
				addressesInTx[addr] = true
			}
			// log.Printf("Address: %s, Amount: %d, Coinbase: %t", addr, amtSat, isCoinbase)
			addrTx := db.AddressTransaction{
				ID:          uuid.New(),
				Address:     addr,
				TxID:        txData.Txid,
				BlockHeight: blockHeight,
				Amount:      amtSat,     // Positive for outputs
				Coinbase:    isCoinbase, // Mark if this is from a coinbase transaction
				InputTxId:   nil,
				InputVout:   nil,
			}

			if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&addrTx).Error; err != nil {
				log.Printf("Error creating address transaction for %s: %v", addr, err)
			}
		}
	}

	return nil
}
