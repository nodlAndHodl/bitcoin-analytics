package blockimporter

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"

	"github.com/btcsuite/btcd/btcjson"

	"github.com/btcsuite/btcd/rpcclient"
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
				log.Printf("Processed block %d/%d (%.2f%%)", height, endHeight, float64(height-startHeight+1)/float64(endHeight-startHeight+1)*100)
			}
		}
	}
}

func (bi *BlockImporter) processBlock(block *btcjson.GetBlockVerboseTxResult) error {
	// Start a DB transaction for this block and its transactions
	dbTx := bi.DB.Begin()
	if dbTx.Error != nil {
		return fmt.Errorf("failed to begin db transaction: %v", dbTx.Error)
	}

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

	// Save block to database, ignore duplicates
	if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(blockRecord).Error; err != nil {
		dbTx.Rollback()
		return fmt.Errorf("failed to save block: %v", err)
	}

	// Process transactions
	for _, txData := range block.Tx {
		if err := bi.processTransaction(dbTx, txData, block.Height, blockTime); err != nil {
			dbTx.Rollback()
			return fmt.Errorf("failed to process transaction %s: %v", txData.Txid, err)
		}
	}

	return dbTx.Commit().Error
}

func (bi *BlockImporter) processTransaction(dbTx *gorm.DB, txData btcjson.TxRawResult, blockHeight int64, blockTime time.Time) error {
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
		BlockTime:   blockTime,
		CreatedAt:   time.Now(),
	}

	// Store inputs and outputs as JSON
	vinJSON, err := json.Marshal(txData.Vin)
	if err != nil {
		return fmt.Errorf("failed to marshal vin: %v", err)
	}
	txRecord.Vin = vinJSON

	voutJSON, err := json.Marshal(txData.Vout)
	if err != nil {
		return fmt.Errorf("failed to marshal vout: %v", err)
	}
	txRecord.Vout = voutJSON

	// Save transaction to database
	if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(txRecord).Error; err != nil {
		return fmt.Errorf("failed to save transaction: %v", err)
	}

	// First, process inputs to handle spends (skip coinbase)
	for _, vin := range txData.Vin {
		if vin.Coinbase != "" {
			continue // coinbase has no spend
		}

		// locate the UTXO being spent
		var utxo db.UTXO
		err := dbTx.Where("tx_id = ? AND vout_index = ?", vin.Txid, vin.Vout).First(&utxo).Error
		if err == nil {
			// debit balance
			if err := dbTx.Exec(`
				UPDATE addresses SET balance = balance - ?, updated_at = NOW() WHERE address = ?`, utxo.Amount, utxo.Address).Error; err != nil {
				return fmt.Errorf("failed to debit balance: %v", err)
			}

			// outgoing address transaction
			addrTx := &db.AddressTransaction{
				Address:     utxo.Address,
				TxID:        txData.Txid,
				BlockHeight: blockHeight,
				Amount:      -utxo.Amount,
				IsOutgoing:  true,
				CreatedAt:   time.Now(),
			}
			_ = dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(addrTx).Error

			// remove utxo
			dbTx.Delete(&utxo)
		}
	}

	// Process outputs to update address balances
	for _, vout := range txData.Vout {
		var addrs []string
		if len(vout.ScriptPubKey.Addresses) > 0 {
			addrs = vout.ScriptPubKey.Addresses
		} else {
			// Legacy scripts (e.g., P2PK) don't include an address list – derive it
			scriptHex := vout.ScriptPubKey.Hex
			scriptBytes, err := hex.DecodeString(scriptHex)
			if err == nil {
				class, extracted, _, err := txscript.ExtractPkScriptAddrs(scriptBytes, &chaincfg.MainNetParams)
				if err == nil {
					if class == txscript.PubKeyTy {
						// keep raw pubkey as identifier (mempool.space style)
						if pushed, err := txscript.PushedData(scriptBytes); err == nil && len(pushed) == 1 {
							addrs = append(addrs, hex.EncodeToString(pushed[0]))
						}
					} else {
						for _, a := range extracted {
							addrs = append(addrs, a.EncodeAddress())
						}
					}
				}
			}
		}

		if len(addrs) == 0 {
			continue // nothing we can attribute
		}

		// Convert amount from BTC to satoshis
		satoshis := int64(vout.Value * 100_000_000)

		for _, addr := range addrs {
			// Update address balance (upsert)
			err := dbTx.Exec(`
				INSERT INTO addresses (address, balance, tx_count, created_at, updated_at)
				VALUES (?, ?, 1, NOW(), NOW())
				ON CONFLICT (address) 
				DO UPDATE SET 
				  balance = addresses.balance + ?,
				  tx_count = addresses.tx_count + 1,
				  updated_at = NOW()
			`, addr, satoshis, satoshis).Error

			if err != nil {
				return fmt.Errorf("failed to update address balance: %v", err)
			}

			// Record address transaction (incoming)
			addrTx := &db.AddressTransaction{
				Address:     addr,
				TxID:        txData.Txid,
				BlockHeight: blockHeight,
				Amount:      satoshis,
				IsOutgoing:  false, // This is a receive transaction
				CreatedAt:   time.Now(),
			}
			if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(addrTx).Error; err != nil {
				return fmt.Errorf("failed to record address transaction: %v", err)
			}

			// insert new utxo
			utxo := &db.UTXO{
				TxID:      txData.Txid,
				VoutIndex: vout.N,
				Address:   addr,
				Amount:    satoshis,
				CreatedAt: time.Now(),
			}
			_ = dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(utxo).Error
		}
	}

	return nil
}
