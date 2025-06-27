package db

import (
	"log"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Block represents a Bitcoin block
type Block struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	Height            int64     `gorm:"uniqueIndex;not null"`
	Hash              string    `gorm:"uniqueIndex;not null"`
	Version           int32     `gorm:"not null"`
	VersionHex        string    `gorm:"not null"`
	MerkleRoot        string    `gorm:"not null"`
	BlockTime         time.Time `gorm:"not null;index"`
	Nonce             uint32    `gorm:"not null"`
	Bits              string    `gorm:"not null"`
	Difficulty        float64   `gorm:"not null"`
	Chainwork         string    `gorm:"not null"`
	NTx               int       `gorm:"not null"`
	PreviousBlockHash string    `gorm:"not null"`
	NextBlockHash     string    `gorm:"not null"`
	StrippedSize      int       `gorm:"not null"`
	Size              int       `gorm:"not null"`
	Weight            int       `gorm:"not null"`
}

// Transaction represents a Bitcoin transaction
type Transaction struct {
	ID          uuid.UUID      `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	BlockHeight int64          `gorm:"not null;index"`
	Hex         string         `gorm:"not null"`
	Txid        string         `gorm:"uniqueIndex;not null"`
	Hash        string         `gorm:"not null"`
	Size        int            `gorm:"not null"`
	Vsize       int            `gorm:"not null"`
	Weight      int            `gorm:"not null"`
	Version     int32          `gorm:"not null"`
	Locktime    uint32         `gorm:"not null"`
	Vin         datatypes.JSON `gorm:"type:jsonb"`
	Vout        datatypes.JSON `gorm:"type:jsonb"`
	BlockTime   time.Time      `gorm:"not null;index"`
}

// Address represents data from the address_balances view
type Address struct {
	Address          string `gorm:"primarykey" json:"address"`
	TransactionCount int64  `gorm:"column:tx_count" json:"transaction_count"`
	Balance          int64  `json:"balance"`                                           // In satoshis
	CoinbaseBalance  int64  `gorm:"column:coinbase_balance" json:"coinbase_balance"`   // Mining rewards
	CoinbaseTxCount  int64  `gorm:"column:coinbase_tx_count" json:"coinbase_tx_count"` // Count of mining reward transactions
}

// TableName sets the table name for Address model to use our custom materialized view
func (Address) TableName() string {
	return "address_balances"
}

// AddressTransaction represents a transaction involving a specific address
type AddressTransaction struct {
	ID          uuid.UUID `gorm:"type:uuid;primary_key" json:"id"`
	Address     string    `gorm:"index" json:"address"`
	TxID        string    `gorm:"column:tx_id;index" json:"tx_id"` // Current transaction ID
	BlockHeight int64     `gorm:"index" json:"block_height"`
	Amount      int64     `json:"amount"`                        // In satoshis, can be negative (for inputs/spends)
	Coinbase    bool      `gorm:"default:false" json:"coinbase"` // Whether this is a coinbase transaction (mining reward)

	InputTxId *string `gorm:"column:input_tx_id" json:"input_tx_id"` // For inputs: which tx created the output being spent
	InputVout *int    `gorm:"column:input_vout" json:"input_vout"`   // For inputs: which output index in the origin tx
}

// PricePoint represents OHLC price data
// Stored per hour (or other timeframe) to enable market analytics
// Unique by timestamp + currency
type PricePoint struct {
	ID        uuid.UUID `gorm:"type:uuid;primary_key;"`
	Timestamp time.Time `gorm:"uniqueIndex:idx_timestamp_currency;index"`
	Currency  string    `gorm:"uniqueIndex:idx_timestamp_currency"`
	Open      float64
	High      float64
	Low       float64
	Close     float64
}

// BeforeCreate sets UUIDs for PricePoint
func (p *PricePoint) BeforeCreate(tx *gorm.DB) (err error) {
	p.ID = uuid.New()
	return
}

// MigrateModels runs database migrations
func MigrateModels(db *gorm.DB) error {
	// Note: We only auto-migrate actual tables, not materialized views
	models := []interface{}{
		&PricePoint{},
		&Block{},
		&Transaction{},
		&AddressTransaction{},
		// Views are created explicitly below, not via AutoMigrate
	}

	// Enable UUID extension if not exists
	db.Exec("CREATE EXTENSION IF NOT EXISTS \"uuid-ossp\"")
	db.Exec("CREATE EXTENSION IF NOT EXISTS \"pgcrypto\"")

	// Migrate all models
	for _, model := range models {
		if err := db.AutoMigrate(model); err != nil {
			return err
		}
	}

	// Create only the most essential index needed during import
	db.Exec(`
		-- Critical for transaction lookups during import
		CREATE INDEX IF NOT EXISTS idx_transactions_txid ON transactions(txid);
	`)

	// Create view for address balances
	db.Exec(`
		CREATE OR REPLACE VIEW address_balances AS
		SELECT 
			address,
			COUNT(DISTINCT tx_id) AS tx_count,
			SUM(amount) AS balance,
			SUM(CASE WHEN coinbase = true THEN amount ELSE 0 END) AS coinbase_balance,
			COUNT(DISTINCT CASE WHEN coinbase = true THEN tx_id ELSE NULL END) AS coinbase_tx_count
		FROM 
			address_transactions
		GROUP BY 
			address
		ORDER BY 
			balance DESC;
	`)

	return nil
}

// Views are automatically updated - no refresh functions needed

// CreatePostImportIndexes creates performance-oriented indexes after the initial block import
// This is separated from the initial migration to speed up the import process
func CreatePostImportIndexes(db *gorm.DB) error {
	// Log start of index creation
	log.Println("Creating post-import performance indexes...")

	// Create performance indexes
	db.Exec(`
		-- Standard query indexes
		CREATE INDEX IF NOT EXISTS idx_blocks_time ON blocks(block_time);
		CREATE INDEX IF NOT EXISTS idx_transactions_block_height ON transactions(block_height);
		CREATE INDEX IF NOT EXISTS idx_address_transactions_address ON address_transactions(address);

		-- Enhanced address transaction indexes for balance lookups
		CREATE INDEX IF NOT EXISTS idx_address_transactions_address_amount ON address_transactions(address, amount);
		CREATE INDEX IF NOT EXISTS idx_address_transactions_blockheight ON address_transactions(block_height);

		-- Skip the expensive GIN indexes as they don't appear to be used in query patterns
		-- Uncomment if you add queries that use JSON operators on these columns
		-- CREATE INDEX IF NOT EXISTS idx_transactions_vin_gin ON transactions USING GIN (vin);
		-- CREATE INDEX IF NOT EXISTS idx_transactions_vout_gin ON transactions USING GIN (vout);
	`)

	log.Println("Post-import indexes created successfully")
	return nil
}
