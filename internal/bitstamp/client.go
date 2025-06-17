package bitstamp

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"
)

const (
	BaseURL = "https://www.bitstamp.net/api/v2"
)

// Client for interacting with the Bitstamp API

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient creates a new Bitstamp API client
func NewClient() *Client {
	return &Client{
		BaseURL:    BaseURL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// OHLCResponse represents the structure of the OHLC data from Bitstamp
type OHLCResponse struct {
	Data struct {
		OHLC []struct {
			Close string `json:"close"`
			High  string `json:"high"`
			Low   string `json:"low"`
			Open  string `json:"open"`
			Time  string `json:"timestamp"`
		} `json:"ohlc"`
	} `json:"data"`
}

// GetOHLC fetches OHLC data for a given currency pair and time range
func (c *Client) GetOHLC(pair string, start, step int64) (*OHLCResponse, error) {
	url := fmt.Sprintf("%s/ohlc/%s/?step=%d&limit=1000&start=%d", c.BaseURL, pair, step, start)

	resp, err := c.HTTPClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error making request to Bitstamp: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("Bitstamp API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var ohlcResponse OHLCResponse
	if err := json.NewDecoder(resp.Body).Decode(&ohlcResponse); err != nil {
		return nil, fmt.Errorf("error decoding Bitstamp OHLC response: %v", err)
	}

	return &ohlcResponse, nil
}
