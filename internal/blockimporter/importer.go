package blockimporter

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
)

type BlockImporter struct {
	DB                      *gorm.DB
	RPC                     *rpcclient.Client
	shutdown                chan struct{}
	OnInitialImportComplete func() // Callback to execute when initial import is complete
	wg                      sync.WaitGroup
}

func NewBlockImporter(db *gorm.DB, rpc *rpcclient.Client) *BlockImporter {
	return &BlockImporter{
		DB:       db,
		RPC:      rpc,
		shutdown: make(chan struct{}),
	}
}

const pollInterval = 5 * time.Minute // how often to check for new blocks after initial sync

var ErrBlockExists = errors.New("block already exists")

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
	var dbBlockHeight int64
	result := bi.DB.Model(&db.Block{}).Select("COALESCE(MAX(height), -1)").Scan(&dbBlockHeight)
	if result.Error != nil {
		return fmt.Errorf("failed to get current height: %v", result.Error)
	}

	// Start importing from the next block (if we are behind)
	startHeight := dbBlockHeight + 1
	if startHeight <= nodeHeight {
		log.Printf("Starting bi-directional block import. DB height: %d, Node height: %d", dbBlockHeight, nodeHeight)
		bi.wg.Add(2)

		// Start historical import (forwards)
		go func() {
			defer bi.wg.Done()
			log.Println("Starting historical-importer (forward sync)...")
			bi.importBlocks(startHeight, nodeHeight, "forward")
			log.Println("Historical-importer finished.")
		}()

		// Start recent import (backwards)
		go func() {
			defer bi.wg.Done()
			log.Println("Starting syncing-importer (backward sync)...")
			bi.importBlocks(nodeHeight, startHeight, "backward")
			log.Println("Syncing-importer finished.")
		}()

		bi.wg.Wait() // Wait for both importers to meet in the middle
		log.Println("Initial bi-directional sync complete.")

	} else {
		log.Printf("already at latest block height: %d", dbBlockHeight)
	}

	if bi.OnInitialImportComplete != nil {
		bi.OnInitialImportComplete()
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
					bi.importBlocks(dbTip+1, nodeTip, "forward")
				}
			}
		}
	}()

	return nil
}

// Stop signals the importer to shut down
func (bi *BlockImporter) Stop() {
	log.Println("Shutting down block importer...")
	bi.RPC.Shutdown()
	close(bi.shutdown)
}

func (bi *BlockImporter) importBlocks(startHeight, endHeight int64, direction string) {
	processHeight := func(height int64) bool { // Return true to stop
		select {
		case <-bi.shutdown:
			log.Println("Block import stopped by shutdown signal")
			return true
		default:
			hash, err := bi.RPC.GetBlockHash(height)
			if err != nil {
				log.Printf("Error getting block hash at height %d: %v", height, err)
				return false // Continue to next block
			}
			// Get block with verbose transaction data
			block, err := bi.RPC.GetBlockVerboseTx(hash)
			if err != nil {
				log.Printf("Error getting block at height %d: %v", height, err)
				return false // Continue
			}

			log.Printf("Processing block %d", height)
			if err := bi.processBlock(block); err != nil {
				if errors.Is(err, ErrBlockExists) {
					log.Printf("[%s-importer] Halting at block %d: already imported.", direction, height)
					return true // Stop
				}
				log.Printf("Error processing block %d: %v", height, err)
				return false // Continue
			}

			if height%1000 == 0 {
				log.Printf("[%s-importer] Processed block %d", direction, height)
			}
		}
		return false
	}

	if direction == "forward" {
		for height := startHeight; height <= endHeight; height++ {
			if processHeight(height) {
				break
			}
		}
	} else if direction == "backward" {
		for height := startHeight; height >= endHeight; height-- {
			if processHeight(height) {
				break
			}
		}
	}
}

func (bi *BlockImporter) processBlock(block *btcjson.GetBlockVerboseTxResult) error {
	// Convert block time to time.Time
	blockTime := time.Unix(block.Time, 0)

	// Create block record
	blockRecord := &db.Block{
		Height:            block.Height,
		Hash:              block.Hash,
		Version:           int32(block.Version),
		VersionHex:        fmt.Sprintf("%08x", block.Version),
		MerkleRoot:        block.MerkleRoot,
		BlockTime:         blockTime,
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

	// Use a transaction for atomicity
	err := bi.DB.Transaction(func(tx *gorm.DB) error {
		// Save the block record
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "height"}},
			DoNothing: true,
		}).Create(blockRecord)

		if result.Error != nil {
			return fmt.Errorf("failed to save block: %v", result.Error)
		}

		// If RowsAffected is 0, it means the block already exists, and we can stop this importer.
		if result.RowsAffected == 0 {
			return ErrBlockExists
		}

		// Process each transaction in the block
		for _, txData := range block.Tx {
			err := bi.processTransaction(tx, txData, block.Height)
			if err != nil {
				return fmt.Errorf("failed to process transaction: %v", err)
			}
		}

		return nil
	})

	return err
}

func extractAddresses(scriptPubkey *btcjson.ScriptPubKeyResult) []string {
	var addresses []string
	if len(scriptPubkey.Addresses) > 0 {
		addresses = scriptPubkey.Addresses
	} else if scriptPubkey.Hex != "" {
		// Parse the script to get addresses
		script, err := hex.DecodeString(scriptPubkey.Hex)
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
	return addresses
}

func (bi *BlockImporter) processTransaction(dbTx *gorm.DB, txData btcjson.TxRawResult, blockHeight int64) error {
	// A coinbase transaction has exactly one input, and the input's `Coinbase` field is not empty.
	isCoinbase := len(txData.Vin) == 1 && txData.Vin[0].Coinbase != ""

	// Keep track of addresses seen in this transaction to avoid duplicate address_in_tx records
	addressesInTx := make(map[string]bool)

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

	if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(txRecord).Error; err != nil {
		return fmt.Errorf("failed to save transaction: %v", err)
	}

	// Process inputs - create negative address transactions for spent outputs
	for _, input := range txData.Vin {
		if isCoinbase {
			continue
		}

		var vouts []btcjson.Vout

		// We need to find the referenced transaction output information.
		// First, check our own database.
		var referencedTx db.Transaction
		result := dbTx.Where("txid = ?", input.Txid).First(&referencedTx)

		if result.Error == nil {
			// Transaction found in our database.
			if err := json.Unmarshal(referencedTx.Vout, &vouts); err != nil {
				return fmt.Errorf("failed to unmarshal vout for tx %s from db: %w", input.Txid, err)
			}
		} else if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			// Transaction not in DB, fetch from RPC. This is expected during backward sync.
			txHash, err := chainhash.NewHashFromStr(input.Txid)
			if err != nil {
				return fmt.Errorf("failed to create hash from txid string '%s': %w", input.Txid, err)
			}
			refTxVerbose, err := bi.RPC.GetRawTransactionVerbose(txHash)
			if err != nil {
				// This can happen if the node is pruned and doesn't have the full tx history.
				// We log a warning and skip this input, as we can't process it.
				log.Printf("WARN: could not fetch referenced tx %s from RPC: %v. Skipping input.", input.Txid, err)
				continue
			}
			vouts = refTxVerbose.Vout
		} else {
			// A different database error occurred.
			return fmt.Errorf("db error fetching referenced tx %s: %w", input.Txid, result.Error)
		}

		// Ensure the vout index is valid.
		if int(input.Vout) >= len(vouts) {
			return fmt.Errorf("vout index %d out of range for tx %s (len=%d)", input.Vout, input.Txid, len(vouts))
		}

		// Get the specific output being spent.
		output := vouts[input.Vout]

		// Extract address(es) from the output's script.
		addresses := extractAddresses(&output.ScriptPubKey)
		amtSat := int64(output.Value * 100000000)

		// Create negative address transaction records for each address.
		for _, addr := range addresses {
			if _, seen := addressesInTx[addr]; !seen {
				addressesInTx[addr] = true
			}

			addrTx := db.AddressTransaction{
				ID:          uuid.New(),
				Address:     addr,
				TxID:        txData.Txid,
				BlockHeight: blockHeight,
				Amount:      -amtSat, // Negative for inputs/spends.
				Coinbase:    false,
				InputTxId:   &input.Txid,
				InputVout:   func() *int { v := int(input.Vout); return &v }(),
			}

			if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&addrTx).Error; err != nil {
				return fmt.Errorf("failed to create spend address transaction for %s: %w", addr, err)
			}
		}
	}

	// Process outputs to create address_transactions
	for _, output := range txData.Vout {
		if output.Value <= 0 {
			continue
		}

		amtSat := int64(output.Value * 100000000)
		addresses := extractAddresses(&output.ScriptPubKey)

		for _, addr := range addresses {
			if _, seen := addressesInTx[addr]; !seen {
				addressesInTx[addr] = true
			}
			addrTx := db.AddressTransaction{
				ID:          uuid.New(),
				Address:     addr,
				TxID:        txData.Txid,
				BlockHeight: blockHeight,
				Amount:      amtSat, // Positive for outputs.
				Coinbase:    isCoinbase,
				InputTxId:   nil,
				InputVout:   nil,
			}

			if err := dbTx.Clauses(clause.OnConflict{DoNothing: true}).Create(&addrTx).Error; err != nil {
				return fmt.Errorf("failed to create address transaction for %s: %w", addr, err)
			}
		}
	}

	return nil
}

// ... (rest of the code remains the same)
