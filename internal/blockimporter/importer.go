package blockimporter

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/btcjson"
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
		
		// After initial import, process addresses and UTXOs (second pass)
		// We use max(0, startHeight) because startHeight could be 0 and we need to process from genesis
		processStartHeight := startHeight
		if processStartHeight < 0 {
			processStartHeight = 0
		}
		log.Printf("Starting second-pass processing for addresses and UTXOs from height %d to %d", processStartHeight, nodeHeight)
		if err := bi.ProcessAddressesAndUTXOs(processStartHeight, nodeHeight); err != nil {
			log.Printf("Error in second-pass processing: %v", err)
		}
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
		CreatedAt:         time.Now(),
	}

	// Store transaction hashes as JSON
	txHashes := make([]string, len(block.Tx))
	for i, tx := range block.Tx {
		txHashes[i] = tx.Txid
	}
	txJSON, err := json.Marshal(txHashes)
	if err != nil {
		return fmt.Errorf("failed to marshal tx hashes: %v", err)
	}
	blockRecord.Tx = txJSON

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

	// Batch collections
	var addrTxBatch []*db.AddressTransaction
	var utxoBatch []*db.UTXO

	// Process each transaction in the block
	// Use the transaction data already provided by GetBlockVerboseTx
	// This eliminates the need for additional RPC calls
	for _, txData := range block.Tx {
		// Process transaction - first pass only stores transaction data
		err := bi.processTransaction(dbTx, txData, block.Height, &addrTxBatch, &utxoBatch)
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
	AddrTxBatchSize  = 1000 // Increased batch size for better performance
	UTXOBatchSize    = 1000
	AddressBatchSize = 1000
	BlockBatchSize   = 100  // Process this many blocks before processing their addresses
	MaxHeight       = -1   // Used to process all available blocks
)

// First-pass processing: Store only blocks and transaction data (no addresses or UTXOs)
func (bi *BlockImporter) processTransaction(dbTx *gorm.DB, txData btcjson.TxRawResult, blockHeight int64, addrTxBatch *[]*db.AddressTransaction, utxoBatch *[]*db.UTXO) error {
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
		Locktime:    txData.LockTime,
		BlockTime:   time.Unix(txData.Time, 0),
		CreatedAt:   time.Now(),
	}

	// Serialize vin/vout JSON
	vinJSON, err := json.Marshal(txData.Vin)
	if err != nil {
		return fmt.Errorf("failed to marshal vin data: %v", err)
	}
	txRecord.Vin = vinJSON

	voutJSON, err := json.Marshal(txData.Vout)
	if err != nil {
		return fmt.Errorf("failed to marshal vout data: %v", err)
	}
	txRecord.Vout = voutJSON

	// Save transaction, ignore duplicates
	if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(txRecord).Error; err != nil {
		return fmt.Errorf("failed to save transaction: %v", err)
	}

	// In the first pass we skip address and UTXO processing
	// This is handled in a second pass by ProcessAddressesAndUTXOs

	return nil
}

// ProcessAddressesAndUTXOs is the second pass of the two-pass import
// It processes all transactions to extract addresses, create UTXOs and update address balances
// This should be called after all blocks and transactions are imported
func (bi *BlockImporter) ProcessAddressesAndUTXOs(startHeight, endHeight int64) error {
	log.Printf("Starting second-pass processing for addresses and UTXOs from block %d to %d", startHeight, endHeight)
	
	// If endHeight is -1 (MaxHeight), get the actual highest block height from the database
	if endHeight == MaxHeight {
		var maxHeight int64
		result := bi.DB.Model(&db.Block{}).Select("COALESCE(MAX(height), 0)").Scan(&maxHeight)
		if result.Error != nil {
			return fmt.Errorf("failed to get max block height: %v", result.Error)
		}
		endHeight = maxHeight
		log.Printf("Using maximum available block height: %d", endHeight)
	}

	// Process blocks in batches to avoid memory issues
	for batchStart := startHeight; batchStart <= endHeight; batchStart += BlockBatchSize {
		batchEnd := batchStart + BlockBatchSize - 1
		if batchEnd > endHeight {
			batchEnd = endHeight
		}
		
		// Process this batch of blocks
		if err := bi.processBatchAddressesAndUTXOs(batchStart, batchEnd); err != nil {
			log.Printf("Error processing batch %d-%d: %v", batchStart, batchEnd, err)
			return err
		}
		
		// Log progress
		log.Printf("Processed blocks %d-%d (%.2f%%)", 
			batchStart, batchEnd, float64(batchEnd-startHeight+1)/float64(endHeight-startHeight+1)*100)
	}
	
	log.Printf("Completed second-pass processing for addresses and UTXOs")
	return nil
}

// processBatchAddressesAndUTXOs processes a batch of blocks to extract addresses and build UTXOs
func (bi *BlockImporter) processBatchAddressesAndUTXOs(startHeight, endHeight int64) error {
	// Start a DB transaction for this batch
	dbTx := bi.DB.Begin()
	if dbTx.Error != nil {
		return fmt.Errorf("failed to begin db transaction: %v", dbTx.Error)
	}
	
	// Handle any panics and rollback
	defer func() {
		if r := recover(); r != nil {
			dbTx.Rollback()
			log.Printf("recovered from panic in processBatchAddressesAndUTXOs: %v", r)
		}
	}()
	
	// Initialize batch collections
	addrTxBatch := make([]*db.AddressTransaction, 0, AddrTxBatchSize)
	utxoBatch := make([]*db.UTXO, 0, UTXOBatchSize)
	addrBatch := make(map[string]*db.Address) // Use map to avoid duplicates within batch
	
	// Fetch all transactions for blocks in this batch
	var transactions []db.Transaction
	result := dbTx.Where("block_height BETWEEN ? AND ?", startHeight, endHeight).Order("block_height ASC, id ASC").Find(&transactions)
	if result.Error != nil {
		dbTx.Rollback()
		return fmt.Errorf("failed to fetch transactions: %v", result.Error)
	}
	
	log.Printf("Processing %d transactions for blocks %d-%d", len(transactions), startHeight, endHeight)
	
	// Process each transaction
	for _, tx := range transactions {
		// Process this transaction's addresses and UTXOs
		if err := bi.extractAddressesAndUTXOs(dbTx, &tx, &addrTxBatch, &utxoBatch, addrBatch); err != nil {
			dbTx.Rollback()
			return fmt.Errorf("failed to process transaction %s: %v", tx.Txid, err)
		}
		
		// Flush batches if they're full
		if len(addrTxBatch) >= AddrTxBatchSize {
			if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&addrTxBatch).Error; err != nil {
				dbTx.Rollback()
				return fmt.Errorf("failed to insert address transactions batch: %v", err)
			}
			addrTxBatch = addrTxBatch[:0] // Clear batch but keep capacity
		}
		
		if len(utxoBatch) >= UTXOBatchSize {
			if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&utxoBatch).Error; err != nil {
				dbTx.Rollback()
				return fmt.Errorf("failed to insert UTXOs batch: %v", err)
			}
			utxoBatch = utxoBatch[:0] // Clear batch but keep capacity
		}
		
		// Insert addresses in batches
		if len(addrBatch) >= AddressBatchSize {
			addresses := make([]*db.Address, 0, len(addrBatch))
			for _, addr := range addrBatch {
				addresses = append(addresses, addr)
			}
			
			if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&addresses).Error; err != nil {
				dbTx.Rollback()
				return fmt.Errorf("failed to insert addresses batch: %v", err)
			}
			// Clear the batch
			addrBatch = make(map[string]*db.Address)
		}
	}
	
	// Insert any remaining items in batches
	if len(addrTxBatch) > 0 {
		if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&addrTxBatch).Error; err != nil {
			dbTx.Rollback()
			return fmt.Errorf("failed to insert remaining address transactions: %v", err)
		}
	}
	
	if len(utxoBatch) > 0 {
		if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&utxoBatch).Error; err != nil {
			dbTx.Rollback()
			return fmt.Errorf("failed to insert remaining UTXOs: %v", err)
		}
	}
	
	if len(addrBatch) > 0 {
		addresses := make([]*db.Address, 0, len(addrBatch))
		for _, addr := range addrBatch {
			addresses = append(addresses, addr)
		}
		
		if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&addresses).Error; err != nil {
			dbTx.Rollback()
			return fmt.Errorf("failed to insert remaining addresses: %v", err)
		}
	}
	
	// Update all address balances for addresses in this batch
	if err := bi.updateAddressBalances(dbTx, startHeight, endHeight); err != nil {
		dbTx.Rollback()
		return fmt.Errorf("failed to update address balances: %v", err)
	}
	
	// Commit the transaction
	if err := dbTx.Commit().Error; err != nil {
		return fmt.Errorf("failed to commit batch: %v", err)
	}
	
	return nil
}

// extractAddressesAndUTXOs extracts addresses from transaction inputs and outputs
// and creates UTXO records for each output
func (bi *BlockImporter) extractAddressesAndUTXOs(dbTx *gorm.DB, tx *db.Transaction, addrTxBatch *[]*db.AddressTransaction, utxoBatch *[]*db.UTXO, addrBatch map[string]*db.Address) error {
	// Parse vout data to extract addresses and create UTXOs
	var vout []btcjson.Vout
	if err := json.Unmarshal(tx.Vout, &vout); err != nil {
		return fmt.Errorf("failed to unmarshal vout data: %v", err)
	}

	// Process outputs to create UTXOs and extract addresses
	for voutIdx, output := range vout {
		// Skip outputs with no value
		if output.Value <= 0 {
			continue
		}

		// Convert from BTC to satoshis (100,000,000 satoshis = 1 BTC)
		amtSat := int64(output.Value * 100000000)
		
		// Extract addresses from output
		for _, addr := range output.ScriptPubKey.Addresses {
			// Add the address to the batch if not already exists
			if _, exists := addrBatch[addr]; !exists {
				addrBatch[addr] = &db.Address{
					Address:   addr,
					Balance:   0, // Will be updated later by aggregation
					CreatedAt: time.Now(),
				}
			}

			// Create UTXO for this output
			// Make sure we use uint32 for VoutIndex as per model definition
			utxo := &db.UTXO{
				ID:        uuid.New(),
				TxID:      tx.Txid,
				VoutIndex: uint32(voutIdx),
				Address:   addr,
				Amount:    amtSat,
				CreatedAt: time.Now(),
			}

			// Create address transaction record
			addrTx := &db.AddressTransaction{
				Address:     addr,
				TxID:        tx.Txid,
				BlockHeight: tx.BlockHeight,
				Amount:      amtSat, // Positive for outputs
				CreatedAt:   time.Now(),
			}

			// Add to batches
			*addrTxBatch = append(*addrTxBatch, addrTx)
			*utxoBatch = append(*utxoBatch, utxo)
		}
	}

	// Parse vin data to handle spent UTXOs and create negative address transactions
	var vin []btcjson.Vin
	if err := json.Unmarshal(tx.Vin, &vin); err != nil {
		return fmt.Errorf("failed to unmarshal vin data: %v", err)
	}

	// Process inputs (remove spent UTXOs and create negative address transactions)
	for _, input := range vin {
		// Skip coinbase transactions
		if input.IsCoinBase() {
			continue
		}

		// Find the previous output UTXO
		var prevUTXO db.UTXO
		result := dbTx.Where("tx_id = ? AND vout_index = ?", input.Txid, input.Vout).First(&prevUTXO)
		if result.Error != nil {
			// In the two-pass architecture, all UTXOs should exist at this point
			// Log this as information but don't fail - all transactions should be imported in order
			log.Printf("Info: UTXO not found for input %s:%d in tx %s, skipping", input.Txid, input.Vout, tx.Txid)
			continue
		}

		// Since our UTXO model doesn't track spent status, we need to remove the UTXO when spent
		// Delete the UTXO since it's been spent
		if err := dbTx.Delete(&prevUTXO).Error; err != nil {
			return fmt.Errorf("failed to delete spent UTXO: %v", err)
		}

		// Create negative address transaction for the spent amount
		addrTx := &db.AddressTransaction{
			Address:     prevUTXO.Address,
			TxID:        tx.Txid,
			BlockHeight: tx.BlockHeight,
			Amount:      -prevUTXO.Amount, // Negative for inputs
			CreatedAt:   time.Now(),
		}

		// Add to batch
		*addrTxBatch = append(*addrTxBatch, addrTx)
	}

	return nil
}

// updateAddressBalances updates address balances based on transaction data for the specified block range
func (bi *BlockImporter) updateAddressBalances(dbTx *gorm.DB, startHeight, endHeight int64) error {
	log.Printf("Updating address balances for blocks %d-%d", startHeight, endHeight)

	// Use efficient SQL to update address balances based on the sum of address transactions
	// This is much faster than processing each address individually
	updateQuery := `
	UPDATE addresses a
	SET balance = (
		SELECT COALESCE(SUM(amount), 0)
		FROM address_transactions
		WHERE address = a.address
	)
	WHERE a.address IN (
		SELECT DISTINCT address
		FROM address_transactions
		WHERE block_height BETWEEN ? AND ?
	)
	`

	result := dbTx.Exec(updateQuery, startHeight, endHeight)
	if result.Error != nil {
		return fmt.Errorf("failed to update address balances: %v", result.Error)
	}

	log.Printf("Updated balances for addresses affected by blocks %d-%d", startHeight, endHeight)
	return nil
}
