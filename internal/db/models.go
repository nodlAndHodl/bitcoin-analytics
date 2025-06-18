package db

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Block represents a Bitcoin block
// Block represents a Bitcoin block
type Block struct {
	ID                uuid.UUID      `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	Height            int64          `gorm:"uniqueIndex;not null"`
	Hash              string         `gorm:"uniqueIndex;not null"`
	Version           int32          `gorm:"not null"`
	VersionHex        string         `gorm:"not null"`
	MerkleRoot        string         `gorm:"not null"`
	Time              time.Time      `gorm:"not null;index"`
	MedianTime        time.Time      `gorm:"not null"`
	Nonce             uint32         `gorm:"not null"`
	Bits              string         `gorm:"not null"`
	Difficulty        float64        `gorm:"not null"`
	Chainwork         string         `gorm:"not null"`
	NTx               int            `gorm:"not null"`
	PreviousBlockHash string         `gorm:"not null"`
	NextBlockHash     string         `gorm:"not null"`
	StrippedSize      int            `gorm:"not null"`
	Size              int            `gorm:"not null"`
	Weight            int            `gorm:"not null"`
	Tx                datatypes.JSON `gorm:"type:jsonb"`
	CreatedAt         time.Time      `gorm:"not null"`
}

// Transaction represents a Bitcoin transaction
type Transaction struct {
	ID            uuid.UUID      `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	BlockID       uuid.UUID      `gorm:"type:uuid;not null;index"`
	BlockHeight   int64          `gorm:"not null;index"`
	Hex           string         `gorm:"not null"`
	Txid          string         `gorm:"uniqueIndex;not null"`
	Hash          string         `gorm:"not null"`
	Size          int            `gorm:"not null"`
	Vsize         int            `gorm:"not null"`
	Weight        int            `gorm:"not null"`
	Version       int32          `gorm:"not null"`
	Locktime      uint32         `gorm:"not null"`
	Vin           datatypes.JSON `gorm:"type:jsonb"`
	Vout          datatypes.JSON `gorm:"type:jsonb"`
	BlockTime     time.Time      `gorm:"not null;index"`
	CreatedAt     time.Time      `gorm:"not null"`
}

// Address represents a Bitcoin address and its balance
type Address struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	Address   string    `gorm:"uniqueIndex;not null"`
	Balance   int64     `gorm:"not null;default:0"` // in satoshis
	TxCount   int64     `gorm:"not null;default:0"`
	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

// AddressTransaction tracks transactions per address
type AddressTransaction struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	Address     string    `gorm:"not null;index"`
	TxID        string    `gorm:"not null;index"`
	BlockHeight int64     `gorm:"not null;index"`
	Amount      int64     `gorm:"not null"` // in satoshis
	IsOutgoing  bool      `gorm:"not null"` // true if this is an output from the address
	CreatedAt   time.Time `gorm:"not null"`
}

// AddressAmount represents cumulative amount by address and block height
// (kept for historical analytics)
type AddressAmount struct {
    ID          uint      `gorm:"primaryKey"`
    Address     string    `gorm:"index;not null"`
    Amount      float64   `gorm:"not null"`
    BlockHeight int       `gorm:"index;not null"`
    BlockTime   time.Time `gorm:"not null"`
    TxID        string    `gorm:"not null"`
    VoutIndex   int       `gorm:"not null"`
    CreatedAt   time.Time
}

// PricePoint represents OHLC price data
// Stored per hour (or other timeframe) to enable market analytics
// Unique by timestamp + currency
type PricePoint struct {
    ID        uuid.UUID `gorm:"type:uuid;primary_key;"`
    CreatedAt time.Time
    UpdatedAt time.Time
    DeletedAt gorm.DeletedAt `gorm:"index"`
    Timestamp time.Time      `gorm:"uniqueIndex:idx_timestamp_currency;index"`
    Currency  string         `gorm:"uniqueIndex:idx_timestamp_currency"`
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

// UTXO represents an unspent transaction output
type UTXO struct {
    ID        uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
    TxID      string    `gorm:"not null;index:idx_utxo_ref"`
    VoutIndex uint32    `gorm:"not null;index:idx_utxo_ref"`
    Address   string    `gorm:"not null;index"`
    Amount    int64     `gorm:"not null"` // in satoshis
    CreatedAt time.Time `gorm:"not null"`
}

// MigrateModels runs database migrations
func MigrateModels(db *gorm.DB) error {
	models := []interface{}{
		&AddressAmount{},
		&PricePoint{},
		&Block{},
		&Transaction{},
		&Address{},
		&AddressTransaction{},
        &UTXO{},
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

	// Create indexes
	db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_blocks_time ON blocks(time);
		CREATE INDEX IF NOT EXISTS idx_transactions_block_height ON transactions(block_height);
		CREATE INDEX IF NOT EXISTS idx_address_transactions_address ON address_transactions(address);
		CREATE INDEX IF NOT EXISTS idx_address_transactions_txid ON address_transactions(tx_id);
	`)

	return nil
}
