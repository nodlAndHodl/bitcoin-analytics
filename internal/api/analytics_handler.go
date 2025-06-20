package api

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nodlAndHodl/bitcoin-analytics/internal/analytics"
)

// AnalyticsHandler handles API requests for analytics data.
type AnalyticsHandler struct {
	Service *analytics.Service
}

// NewAnalyticsHandler creates a new handler for analytics endpoints.
func NewAnalyticsHandler(service *analytics.Service) *AnalyticsHandler {
	return &AnalyticsHandler{Service: service}
}

// RegisterRoutes registers the analytics routes with the router.
func (h *AnalyticsHandler) RegisterRoutes(r *gin.Engine) {
	r.GET("/api/v1/analytics/mvrv-zscore", h.GetMVRVZScore)
	r.GET("/api/v1/analytics/supply/weekly", h.GetWeeklySupply)
}

// GetMVRVZScore handles the request to calculate and return the MVRV Z-Score.
func (h *AnalyticsHandler) GetMVRVZScore(c *gin.Context) {
	log.Println("Received request for MVRV Z-Score")

	mvrvData, err := h.Service.CalculateMVRVZScore()
	if err != nil {
		log.Printf("Error calculating MVRV Z-Score: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, mvrvData)
}

// GetWeeklySupply handles the request to return the weekly circulating supply.
func (h *AnalyticsHandler) GetWeeklySupply(c *gin.Context) {
	log.Println("Received request for weekly circulating supply")

	supplyData, err := h.Service.GetWeeklyCirculatingSupply()
	if err != nil {
		log.Printf("Error getting weekly supply: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, supplyData)
}
