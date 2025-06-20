package db

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

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
	CreatedAt   time.Time      `gorm:"not null"`
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

	// Drop existing materialized views and their dependencies in reverse order of creation
	DropStatements := []string{
		"DROP MATERIALIZED VIEW IF EXISTS mv_daily_realized_value;",
		"DROP MATERIALIZED VIEW IF EXISTS mv_daily_circulating_supply;",
		"DROP VIEW IF EXISTS v_all_inputs;",
		"DROP VIEW IF EXISTS v_all_outputs;",
	}

	for _, stmt := range DropStatements {
		if err := db.Exec(stmt).Error; err != nil {
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

	// Create SQL function for block subsidy calculation
	db.Exec(`
		CREATE OR REPLACE FUNCTION calculate_block_subsidy(block_height BIGINT)
		RETURNS BIGINT AS $$
		DECLARE
		    initial_subsidy BIGINT := 5000000000; -- 50 BTC in satoshis
		    halvings INT;
		BEGIN
		    IF block_height < 0 THEN
		        RETURN 0;
		    END IF;
		    halvings := block_height / 210000;
		    IF halvings >= 64 THEN -- Max halvings before subsidy becomes 0
		        RETURN 0;
		    END IF;
		    RETURN initial_subsidy >> halvings; -- Bitwise right shift for halving
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;
	`)

	// Create helper views
	db.Exec(`
		CREATE OR REPLACE VIEW v_all_outputs AS
		SELECT
			t.txid,
			(vout_item ->> 'n')::int AS vout_index,
			t.block_time AS creation_time,
			(vout_item -> 'value')::numeric AS value_btc,
			(SELECT p.close FROM price_points p WHERE p.timestamp <= t.block_time ORDER BY p.timestamp DESC LIMIT 1) AS price_at_creation
		FROM
			transactions t,
			jsonb_array_elements(t.vout) AS vout_item;
	`)

	db.Exec(`
		CREATE OR REPLACE VIEW v_all_inputs AS
		SELECT
			(vin_item ->> 'txid') AS spent_txid,
			(vin_item ->> 'vout')::int AS spent_vout_index,
			t.block_time as spent_time
		FROM
			transactions t,
			jsonb_array_elements(t.vin) as vin_item
		WHERE
			jsonb_extract_path_text(vin_item, 'txid') IS NOT NULL;
	`)

	// Materialized View: Daily Circulating Supply (cumulative total supply and daily transaction fees)
	db.Exec(`
		CREATE MATERIALIZED VIEW mv_daily_circulating_supply AS
		WITH coinbase_transactions_daily AS (
			-- This CTE calculates the daily new supply and daily fees from coinbase transactions
			SELECT
				date_trunc('day', t.block_time AT TIME ZONE 'UTC') AS day,
				SUM(
					(SELECT SUM((vout_item->>'value')::numeric * 100000000)::BIGINT FROM jsonb_array_elements(t.vout) AS vout_item)
				) as daily_new_supply_sats,
				SUM(
					GREATEST(0, 
						(SELECT SUM((vout_item->>'value')::numeric * 100000000)::BIGINT FROM jsonb_array_elements(t.vout) AS vout_item) 
						-
						calculate_block_subsidy(t.block_height)
					)
				) AS daily_block_transaction_fees_sats
			FROM
				transactions t
			WHERE
				EXISTS (
					SELECT 1
					FROM jsonb_array_elements(t.vin) WITH ORDINALITY AS vin_arr(vin_item, vin_idx)
					WHERE vin_arr.vin_idx = 1 AND vin_arr.vin_item ? 'coinbase'
				)
			GROUP BY
				date_trunc('day', t.block_time AT TIME ZONE 'UTC')
		)
		SELECT
			day,
			SUM(daily_new_supply_sats) OVER (ORDER BY day ASC) as circulating_supply_sats, -- Cumulative total supply
			daily_block_transaction_fees_sats as block_transaction_fees_sats -- Daily fees (not cumulative)
		FROM
			coinbase_transactions_daily
		ORDER BY
			day;

		CREATE UNIQUE INDEX IF NOT EXISTS idx_mv_daily_circulating_supply_day ON mv_daily_circulating_supply(day);
	`)

	// Materialized View for Realized Value
	db.Exec(`
		CREATE MATERIALIZED VIEW IF NOT EXISTS mv_daily_realized_value AS
		WITH output_lifecycle AS (
			SELECT
				o.creation_time,
				o.value_btc * o.price_at_creation AS realized_value_contribution,
				i.spent_time
			FROM
				v_all_outputs o
			LEFT JOIN
				v_all_inputs i ON o.txid = i.spent_txid AND o.vout_index = i.spent_vout_index
		),
		daily_changes AS (
			SELECT
				day,
				SUM(value_added) as total_added,
				SUM(value_removed) as total_removed
			FROM (
				SELECT date_trunc('day', creation_time) as day, realized_value_contribution as value_added, 0 as value_removed FROM output_lifecycle
				UNION ALL
				SELECT date_trunc('day', spent_time) as day, 0 as value_added, realized_value_contribution as value_removed FROM output_lifecycle WHERE spent_time IS NOT NULL
			) AS changes
			GROUP BY day
		)
		SELECT
			day,
			SUM(total_added - total_removed) OVER (ORDER BY day ASC) as realized_value
		FROM
			daily_changes
		WHERE day IS NOT NULL
		ORDER BY
			day;
	`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_mv_daily_realized_value_day ON mv_daily_realized_value(day);`)

	return nil
}
