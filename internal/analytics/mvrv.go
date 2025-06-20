package analytics

import (
	"fmt"
	"log"
	"math"
	"time"

	"gorm.io/gorm"
)

// Service handles analytics calculations and data refreshing.
type Service struct {
	DB *gorm.DB
}

// NewService creates a new analytics service.
func NewService(db *gorm.DB) *Service {
	return &Service{DB: db}
}

// RefreshAnalyticsViews refreshes the materialized views used for analytics.
// It uses CONCURRENTLY to avoid locking the views during the refresh.
func (s *Service) RefreshAnalyticsViews() {
	log.Println("Refreshing analytics materialized views...")

	viewsToRefresh := []string{
		"mv_daily_circulating_supply",
		"mv_daily_realized_value",
	}

	for _, viewName := range viewsToRefresh {
		log.Printf("Refreshing %s...", viewName)
		if err := s.DB.Exec(fmt.Sprintf("REFRESH MATERIALIZED VIEW CONCURRENTLY %s;", viewName)).Error; err != nil {
			log.Printf("Error refreshing %s: %v", viewName, err)
		} else {
			log.Printf("Successfully refreshed %s.", viewName)
		}
	}
	log.Println("Finished refreshing analytics materialized views.")
}

// MVRVData holds the final calculated MVRV Z-Score and its components.
type MVRVData struct {
	Day               time.Time `json:"day"`
	MarketValue       float64   `json:"market_value"`
	RealizedValue     float64   `json:"realized_value"`
	MVRVZScore        float64   `json:"mvrv_z_score"`
	StdDevMarketValue float64   `json:"std_dev_market_value"`
}

// DailyMetric is a helper struct to hold combined data for a single day.
type DailyMetric struct {
	Day           time.Time
	MarketValue   float64
	RealizedValue float64
}

// realizedValueData is a helper struct to scan data from the realized value view.
type realizedValueData struct {
	Day           time.Time `gorm:"column:day"`
	RealizedValue float64   `gorm:"column:realized_value"`
}

// circulatingSupplyData is a helper struct to scan data from the supply view.
type circulatingSupplyData struct {
	Day                   time.Time `gorm:"column:day"`
	CirculatingSupplySats int64     `gorm:"column:circulating_supply_sats"` // This is the total daily supply (subsidy + fees)
}

// dailyPrice is a helper struct to scan daily closing prices.
type dailyPrice struct {
	Day   time.Time `gorm:"column:day"`
	Close float64   `gorm:"column:close"`
}

// CalculateMVRVZScore calculates the current MVRV Z-Score by fetching and processing data from the materialized views.
func (s *Service) CalculateMVRVZScore() (*MVRVData, error) {
	// 1. Fetch all daily realized values
	var realizedValues []realizedValueData
	if err := s.DB.Table("mv_daily_realized_value").Order("day asc").Find(&realizedValues).Error; err != nil {
		return nil, fmt.Errorf("failed to fetch realized values: %w", err)
	}
	if len(realizedValues) == 0 {
		return nil, fmt.Errorf("no realized value data found in materialized view")
	}
	realizedValueMap := make(map[string]float64)
	for _, v := range realizedValues {
		realizedValueMap[v.Day.Format("2006-01-02")] = v.RealizedValue
	}

	// 2. Fetch all daily circulating supplies
	var supplyValues []circulatingSupplyData
	if err := s.DB.Table("mv_daily_circulating_supply").Order("day asc").Find(&supplyValues).Error; err != nil {
		return nil, fmt.Errorf("failed to fetch circulating supply: %w", err)
	}
	if len(supplyValues) == 0 {
		return nil, fmt.Errorf("no circulating supply data found in materialized view")
	}
	supplyMap := make(map[string]int64)
	for _, v := range supplyValues {
		supplyMap[v.Day.Format("2006-01-02")] = v.CirculatingSupplySats
	}

	// 3. Fetch daily closing prices using a custom query to get one price per day
	var prices []dailyPrice
	priceQuery := `SELECT date_trunc('day', timestamp) as day, (array_agg(close ORDER BY timestamp DESC))[1] as close FROM price_points WHERE currency = 'usd' GROUP BY day`
	if err := s.DB.Raw(priceQuery).Scan(&prices).Error; err != nil {
		return nil, fmt.Errorf("failed to fetch daily prices: %w", err)
	}
	priceMap := make(map[string]float64)
	for _, p := range prices {
		priceMap[p.Day.Format("2006-01-02")] = p.Close
	}

	// 4. Combine data into a single, ordered time series of daily metrics
	var metrics []DailyMetric
	for _, r := range realizedValues { // Iterate over realized values to maintain order
		dayKey := r.Day.Format("2006-01-02")
		price, priceOk := priceMap[dayKey]
		supply, supplyOk := supplyMap[dayKey]

		if priceOk && supplyOk && price > 0 {
			marketValue := (float64(supply) / 1e8) * price
			metrics = append(metrics, DailyMetric{
				Day:           r.Day,
				MarketValue:   marketValue,
				RealizedValue: r.RealizedValue,
			})
		}
	}

	if len(metrics) < 2 {
		return nil, fmt.Errorf("not enough combined data points to calculate z-score")
	}

	// 5. Calculate standard deviation of market value
	marketValues := make([]float64, len(metrics))
	for i, m := range metrics {
		marketValues[i] = m.MarketValue
	}
	_, stdDev := calculateStdDev(marketValues)
	if stdDev == 0 {
		return nil, fmt.Errorf("standard deviation of market value is zero, cannot calculate z-score")
	}

	// 6. Calculate Z-Score for the latest day
	latestMetric := metrics[len(metrics)-1]
	zScore := (latestMetric.MarketValue - latestMetric.RealizedValue) / stdDev

	return &MVRVData{
		Day:               latestMetric.Day,
		MarketValue:       latestMetric.MarketValue,
		RealizedValue:     latestMetric.RealizedValue,
		MVRVZScore:        zScore,
		StdDevMarketValue: stdDev,
	}, nil
}

// calculateStdDev calculates the mean and standard deviation of a slice of float64s.
func calculateStdDev(data []float64) (mean, stdDev float64) {
	if len(data) < 2 {
		return 0, 0
	}
	sum := 0.0
	for _, v := range data {
		sum += v
	}
	mean = sum / float64(len(data))

	varianceSum := 0.0
	for _, v := range data {
		varianceSum += math.Pow(v-mean, 2)
	}
	variance := varianceSum / float64(len(data)) // Population standard deviation
	stdDev = math.Sqrt(variance)
	return mean, stdDev
}

// WeeklySupplyData holds the block subsidy, miner fees, and total circulating supply for a given week.
type WeeklySupplyData struct {
	Week             time.Time `json:"week"`
	BlockSubsidyBTC  float64   `json:"block_subsidy_btc"`
	MinerFeesBTC     float64   `json:"miner_fees_btc"`
	TotalSupplyBTC   float64   `json:"total_supply_btc"`
}

// GetWeeklyCirculatingSupply fetches the circulating supply aggregated by week, with a breakdown of subsidy and fees.
func (s *Service) GetWeeklyCirculatingSupply() ([]WeeklySupplyData, error) {
	// This struct is used to scan the raw SQL query result.
	type weeklySupplyQueryResult struct {
		WeekStart                      time.Time `gorm:"column:week_start"`
		EndOfWeekTotalSupplySats       int64     `gorm:"column:end_of_week_total_supply_sats"`
		PreviousWeekEndTotalSupplySats int64     `gorm:"column:previous_week_end_total_supply_sats"`
		WeeklyTotalTransactionFeesSats int64     `gorm:"column:weekly_total_transaction_fees_sats"`
	}

	var queryResults []weeklySupplyQueryResult

	// mv_daily_circulating_supply.circulating_supply_sats is now cumulative total supply for that day.
	// mv_daily_circulating_supply.block_transaction_fees_sats is the fees for that specific day.
	query := `
		WITH weekly_aggregated_data AS (
			-- This CTE determines the total supply at the end of each week
			-- and sums all transaction fees collected during each week.
			SELECT
				date_trunc('week', day AT TIME ZONE 'UTC') AS week_start,
				-- Get the cumulative circulating supply from the last day recorded in this week
				(array_agg(circulating_supply_sats ORDER BY day DESC))[1] as end_of_week_total_supply_sats,
				-- Sum all daily transaction fees within this week
				SUM(block_transaction_fees_sats) as weekly_total_transaction_fees_sats
			FROM
				mv_daily_circulating_supply
			GROUP BY
				week_start
		)
		SELECT
			week_start,
			end_of_week_total_supply_sats,
			-- Lag to get the previous week's total supply. COALESCE handles the first week.
			COALESCE(LAG(end_of_week_total_supply_sats, 1) OVER (ORDER BY week_start ASC), 0) as previous_week_end_total_supply_sats,
			weekly_total_transaction_fees_sats
		FROM
			weekly_aggregated_data
		ORDER BY
			week_start ASC;
	`

	if err := s.DB.Raw(query).Scan(&queryResults).Error; err != nil {
		return nil, fmt.Errorf("failed to fetch weekly circulating supply: %w", err)
	}

	if len(queryResults) == 0 {
		return nil, fmt.Errorf("no weekly supply data found, the materialized view might still be populating")
	}

	results := make([]WeeklySupplyData, len(queryResults))
	for i, qr := range queryResults {
		totalSupplyAtWeekEndBTC := float64(qr.EndOfWeekTotalSupplySats) / 1e8
		weeklyMinerFeesBTC := float64(qr.WeeklyTotalTransactionFeesSats) / 1e8

		// Calculate new supply issued during this week
		newSupplyThisWeekSats := qr.EndOfWeekTotalSupplySats - qr.PreviousWeekEndTotalSupplySats
		
		// Calculate block subsidy for this week by subtracting fees from the new supply for the week
		weeklyBlockSubsidySats := newSupplyThisWeekSats - qr.WeeklyTotalTransactionFeesSats
		weeklyBlockSubsidyBTC := float64(weeklyBlockSubsidySats) / 1e8

		// Sanity check: block subsidy should not be negative.
		// This could happen if fee calculation is off or if new supply is unexpectedly low.
		if weeklyBlockSubsidyBTC < 0 {
			log.Printf(
				"Warning: Calculated negative block subsidy for week %s. NewSupplySats: %d, FeesSats: %d. Setting subsidy to 0 and adjusting total supply for consistency.", 
				qr.WeekStart.Format("2006-01-02"), 
				newSupplyThisWeekSats, 
				qr.WeeklyTotalTransactionFeesSats,
			)
			// If subsidy is negative, it implies fees were greater than new supply. This is abnormal for Bitcoin.
			// For reporting, we'll cap subsidy at 0. The total supply reported is the actual end-of-week value.
			weeklyBlockSubsidyBTC = 0 
		}

		results[i] = WeeklySupplyData{
			Week:             qr.WeekStart,
			TotalSupplyBTC:   totalSupplyAtWeekEndBTC,
			MinerFeesBTC:     weeklyMinerFeesBTC,
			BlockSubsidyBTC:  weeklyBlockSubsidyBTC,
		}
	}

	return results, nil
}

// StartRefresher starts a background job to periodically refresh the materialized views.
func (s *Service) StartRefresher(interval time.Duration) {
	log.Printf("Starting analytics view refresher to run every %v", interval)
	ticker := time.NewTicker(interval)

	go func() {
		// Perform an initial refresh immediately on startup
		s.RefreshAnalyticsViews()

		for {
			// Wait for the next tick
			<-ticker.C
			s.RefreshAnalyticsViews()
		}
	}()
}
