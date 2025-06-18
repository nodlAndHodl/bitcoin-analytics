package addressimporter

import (
	"encoding/hex"
	"encoding/json"
	"log"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
)

// Importer scans the existing transactions table and populates the
// addresses and address_transactions tables.  It is intended to be
// run once as a back-fill (e.g. after the block importer has inserted
// historical blocks) and can be re-run safely thanks to ON CONFLICT
// DO NOTHING clauses.
//
// Start() performs all work synchronously; run it in a goroutine if you
// need it to be asynchronous with the rest of the application.
type Importer struct {
	DB *gorm.DB
}

func NewImporter(db *gorm.DB) *Importer {
	return &Importer{DB: db}
}

// Start begins the scan.  It iterates over all transactions that have not
// yet been processed for addresses (determined by absence in
// address_transactions).  If you need faster performance, index
// address_transactions.tx_id or batch by height.
func (im *Importer) Start() error {
	log.Println("Address importer: scanning transactions…")

	// Stream through transactions to avoid high memory usage.
	// The query selects txs whose txid is missing from address_transactions.
	rows, err := im.DB.Model(&db.Transaction{}).
		Select("txid, vout, block_height").
		Joins("LEFT JOIN address_transactions atx ON transactions.txid = atx.tx_id").
		Where("atx.id IS NULL").
		Rows()
	if err != nil {
		return err
	}
	defer rows.Close()

	processed := 0
	for rows.Next() {
		var txid string
		var voutBytes []byte
		var height int64
		if err := rows.Scan(&txid, &voutBytes, &height); err != nil {
			return err
		}

		if err := im.handleOutputs(txid, voutBytes, height); err != nil {
			return err
		}

		processed++
		if processed%10000 == 0 {
			log.Printf("Address importer: processed %d transactions", processed)
		}
	}

	log.Printf("Address importer complete: processed %d transactions", processed)
	return nil
}

// handleOutputs decodes vout JSON and updates address balances + mapping.
func (im *Importer) handleOutputs(txid string, voutBytes []byte, height int64) error {
	var vouts []struct {
		Value        float64 `json:"value"`
		N            uint32  `json:"n"`
		ScriptPubKey struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	}

	if err := json.Unmarshal(voutBytes, &vouts); err != nil {
		return err
	}

	for _, v := range vouts {
		script, err := hex.DecodeString(v.ScriptPubKey.Hex)
		if err != nil {
			continue // skip malformed output
		}

		_, addrs, _, err := txscript.ExtractPkScriptAddrs(script, &chaincfg.MainNetParams)
		if err != nil || len(addrs) == 0 {
			continue // non-standard or anyone-can-spend; ignore
		}

		satoshis := int64(v.Value * 1e8)
		for _, a := range addrs {
			addr := a.EncodeAddress()

			// upsert into addresses table
			if err := im.DB.Exec(`
                INSERT INTO addresses (address, balance, tx_count, updated_at)
                VALUES (?, ?, 1, NOW())
                ON CONFLICT (address) DO UPDATE
                  SET balance = addresses.balance + EXCLUDED.balance,
                      tx_count = addresses.tx_count + 1,
                      updated_at = NOW()`, addr, satoshis).Error; err != nil {
				return err
			}

			// insert address transaction link
			atx := &db.AddressTransaction{
				Address:     addr,
				TxID:        txid,
				BlockHeight: height,
				Amount:      satoshis,
				IsOutgoing:  false,
				CreatedAt:   time.Now(),
			}
			if err := im.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(atx).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
