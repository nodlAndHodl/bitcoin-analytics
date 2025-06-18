package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
		"gorm.io/gorm"

	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
)

type ExplorerHandler struct {
	DB *gorm.DB
}

func NewExplorerHandler(db *gorm.DB) *ExplorerHandler {
	return &ExplorerHandler{DB: db}
}

func (h *ExplorerHandler) RegisterRoutes(router *gin.Engine) {
	api := router.Group("/api/v1/explorer")
	{
		api.GET("/blocks", h.getBlocks)
		api.GET("/blocks/:height", h.getBlockByHeight)
		api.GET("/blocks/hash/:hash", h.getBlockByHash)
		api.GET("/transactions/:txid", h.getTransaction)
		api.GET("/addresses/:address", h.getAddress)
	}
}

// getBlocks returns a paginated list of blocks
func (h *ExplorerHandler) getBlocks(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset := (page - 1) * limit

	if page < 1 {
		page = 1
	}

	if limit < 1 || limit > 100 {
		limit = 20
	}

	var blocks []db.Block
	var count int64

	// Get total count
	h.DB.Model(&db.Block{}).Count(&count)

	// Get paginated blocks
	h.DB.Order("height DESC").Offset(offset).Limit(limit).Find(&blocks)

	c.JSON(http.StatusOK, gin.H{
		"data": blocks,
		"pagination": gin.H{
			"current_page": page,
			"per_page":     limit,
			"total":        count,
			"total_pages":  (int(count) + limit - 1) / limit,
		},
	})
}

// getBlockByHeight returns a block by its height
func (h *ExplorerHandler) getBlockByHeight(c *gin.Context) {
	height, err := strconv.ParseInt(c.Param("height"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid block height"})
		return
	}

	var block db.Block
	if err := h.DB.Where("height = ?", height).First(&block).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Block not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch block"})
		}
		return
	}

	c.JSON(http.StatusOK, block)
}

// getBlockByHash returns a block by its hash
func (h *ExplorerHandler) getBlockByHash(c *gin.Context) {
	hash := c.Param("hash")

	var block db.Block
	if err := h.DB.Where("hash = ?", hash).First(&block).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Block not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch block"})
		}
		return
	}

	c.JSON(http.StatusOK, block)
}

// getTransaction returns a transaction by its ID
func (h *ExplorerHandler) getTransaction(c *gin.Context) {
	txid := c.Param("txid")

	var tx db.Transaction
	if err := h.DB.Where("txid = ?", txid).First(&tx).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Transaction not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch transaction"})
		}
		return
	}

	c.JSON(http.StatusOK, tx)
}

// getAddress returns address information and transactions
func (h *ExplorerHandler) getAddress(c *gin.Context) {
	address := c.Param("address")

	// Get address info
	var addrInfo db.Address
	if err := h.DB.Where("address = ?", address).First(&addrInfo).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusOK, gin.H{
				"address":       address,
				"balance":       0,
				"tx_count":      0,
				"transactions":  []interface{}{},
				"total_received": 0,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch address info"})
		return
	}

	// Get paginated transactions
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset := (page - 1) * limit

	if page < 1 {
		page = 1
	}

	if limit < 1 || limit > 50 {
		limit = 20
	}

	var txs []db.AddressTransaction
	var totalTx int64

	h.DB.Model(&db.AddressTransaction{}).Where("address = ?", address).Count(&totalTx)
	h.DB.Where("address = ?", address).
		Order("created_at DESC").
		Offset(offset).
		Limit(limit).
		Find(&txs)

	c.JSON(http.StatusOK, gin.H{
		"address":        addrInfo.Address,
		"balance":        addrInfo.Balance,
		"tx_count":       addrInfo.TxCount,
		"transactions":   txs,
		"pagination": gin.H{
			"current_page": page,
			"per_page":     limit,
			"total":        totalTx,
			"total_pages":  (int(totalTx) + limit - 1) / limit,
		},
	})
}
