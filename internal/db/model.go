package db

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

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

// PricePoint represents a single data point for the price of a cryptocurrency.
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

// BeforeCreate will set a UUID rather than numeric ID.
func (p *PricePoint) BeforeCreate(tx *gorm.DB) (err error) {
	p.ID = uuid.New()
	return
}

func MigrateModels(db *gorm.DB) error {
	// Migrate all your models here
	return db.AutoMigrate(
		&AddressAmount{},
		&PricePoint{},
	)
}
