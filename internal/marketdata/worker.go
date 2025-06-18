package marketdata

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/nodlAndHodl/bitcoin-analytics/internal/bitstamp"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// Bitstamp provides data from 2011-08-18
	BitcoinGenesisDateString = "2011-08-18"
	HourlyStep               = 3600 // 1 hour in seconds
)

var BitcoinGenesisDate time.Time

func init() {
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		log.Fatalf("Failed to load UTC location: %v", err)
	}
	BitcoinGenesisDate, err = time.ParseInLocation("2006-01-02", BitcoinGenesisDateString, loc)
	if err != nil {
		log.Fatalf("Failed to parse genesis date: %v", err)
	}
}

type Worker struct {
	db                  *gorm.DB
	client              *bitstamp.Client
	currencyPair        string
	initialBackfillDone bool
}

func PriceWorker(db *gorm.DB) *Worker {
	return &Worker{
		db:                  db,
		client:              bitstamp.BitstampClient(),
		currencyPair:        "btcusd",
		initialBackfillDone: false,
	}
}

func (w *Worker) Start() {
	log.Println("Starting market data worker...")

	// Run initial backfill synchronously to prevent race conditions
	if err := w.BackfillHistoricalData(); err != nil {
		log.Printf("Initial backfill failed: %v", err)
	}

	// Initial fetch to ensure data is up-to-date right away
	if err := w.FetchAndStore(); err != nil {
		log.Printf("Error fetching recent market data: %v", err)
	}

	// Periodically fetch recent data
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		if err := w.FetchAndStore(); err != nil {
			log.Printf("Error fetching recent market data: %v", err)
		}
	}
}

func (w *Worker) BackfillHistoricalData() error {
	if w.initialBackfillDone {
		return nil
	}

	log.Println("Checking if historical data backfill is needed...")

	var lastPoint db.PricePoint
	result := w.db.Where("timestamp < ?", time.Now().Add(-1*time.Hour)).Order("timestamp desc").First(&lastPoint)

	startDate := BitcoinGenesisDate
	if result.Error == nil {
		// Start from the hour after the last point we have
		startDate = lastPoint.Timestamp.Add(time.Hour)
	} else if result.Error != gorm.ErrRecordNotFound {
		return fmt.Errorf("error fetching last price point: %v", result.Error)
	}

	if !startDate.Before(time.Now().UTC()) {
		log.Println("Price data is already up to date. Skipping backfill.")
		w.initialBackfillDone = true
		return nil
	}

	log.Printf("Starting historical data backfill from %s...", startDate.Format("2006-01-02"))

	currentDate := startDate
	for currentDate.Before(time.Now().UTC()) {
		log.Printf("Fetching hourly data starting from %s...", currentDate.Format("2006-01-02 15:04:05"))
		ohlcData, err := w.client.GetOHLC(w.currencyPair, currentDate.Unix(), HourlyStep)
		if err != nil {
			return fmt.Errorf("error fetching historical OHLC data: %v", err)
		}

		if len(ohlcData.Data.OHLC) == 0 {
			log.Println("No more historical data to fetch. Backfill complete.")
			break
		}

		if err := w.storeOHLCData(ohlcData); err != nil {
			return err
		}

		// Update start date to the timestamp of the last record
		lastTimestamp, err := strconv.ParseInt(ohlcData.Data.OHLC[len(ohlcData.Data.OHLC)-1].Time, 10, 64)
		if err != nil {
			return fmt.Errorf("could not parse last timestamp: %v", err)
		}
		currentDate = time.Unix(lastTimestamp, 0).UTC().Add(time.Hour)

		// Be nice to the API
		time.Sleep(1 * time.Second)
	}

	log.Println("Historical data backfill completed.")
	w.initialBackfillDone = true
	return nil
}

// storeOHLCData stores OHLC data in the database
func (w *Worker) storeOHLCData(data *bitstamp.OHLCResponse) error {
	pricePoints := make([]db.PricePoint, 0, len(data.Data.OHLC))
	for _, ohlc := range data.Data.OHLC {
		ts, err := strconv.ParseInt(ohlc.Time, 10, 64)
		if err != nil {
			log.Printf("Skipping record with invalid timestamp: %v", ohlc)
			continue
		}
		open, err := strconv.ParseFloat(ohlc.Open, 64)
		if err != nil {
			log.Printf("Skipping record with invalid open price: %v", ohlc)
			continue
		}
		high, err := strconv.ParseFloat(ohlc.High, 64)
		if err != nil {
			log.Printf("Skipping record with invalid high price: %v", ohlc)
			continue
		}
		low, err := strconv.ParseFloat(ohlc.Low, 64)
		if err != nil {
			log.Printf("Skipping record with invalid low price: %v", ohlc)
			continue
		}
		close, err := strconv.ParseFloat(ohlc.Close, 64)
		if err != nil {
			log.Printf("Skipping record with invalid close price: %v", ohlc)
			continue
		}

		// Bitstamp may return an empty data point for the current, unclosed candle
		if open == 0 && high == 0 && low == 0 && close == 0 {
			continue
		}

		pricePoints = append(pricePoints, db.PricePoint{
			Timestamp: time.Unix(ts, 0).UTC(),
			Currency:  "usd",
			Open:      open,
			High:      high,
			Low:       low,
			Close:     close,
		})
	}

	if len(pricePoints) == 0 {
		return nil
	}

	tx := w.db.Begin()
	if tx.Error != nil {
		return fmt.Errorf("failed to begin transaction: %w", tx.Error)
	}

	result := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "timestamp"}, {Name: "currency"}},
		DoUpdates: clause.AssignmentColumns([]string{"open", "high", "low", "close"}),
	}).CreateInBatches(pricePoints, 200)

	if result.Error != nil {
		tx.Rollback()
		return fmt.Errorf("failed to batch insert price points: %w", result.Error)
	}

	log.Printf("Successfully inserted/updated %d price points.", result.RowsAffected)

	return tx.Commit().Error
}

func (w *Worker) FetchAndStore() error {
	log.Println("Fetching recent hourly OHLC data...")
	// Fetch data for the last 2 hours to ensure we are up-to-date
	start := time.Now().Add(-2 * time.Hour).Unix()
	data, err := w.client.GetOHLC(w.currencyPair, start, HourlyStep)
	if err != nil {
		return fmt.Errorf("error fetching recent hourly OHLC data: %v", err)
	}

	return w.storeOHLCData(data)
}
