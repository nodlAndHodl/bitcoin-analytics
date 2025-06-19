package peercrawler

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
	"golang.org/x/net/proxy"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/nodlAndHodl/bitcoin-analytics/internal/db"
)

// maxPeers limits how many peers we keep queued at once to avoid runaway memory use.
const maxPeers = 10000

// crawlInterval decides how often we try to revisit a known peer.
const crawlInterval = 6 * time.Hour

// peerTimeout is the TCP + protocol handshake timeout.
const peerTimeout = 30 * time.Second // Increased timeout

// defaultTorProxy is where the local Tor SOCKS5 proxy usually listens.
var defaultTorProxy = "127.0.0.1:9050"

// Alternative Tor ports to try if the default port fails
var alternativeTorPorts = []string{"8333", "8334", "18333", "18444", "28333"}

// isTorProxyAvailable checks if the Tor SOCKS5 proxy is available
func isTorProxyAvailable(proxyAddr string) bool {
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		log.Printf("crawler: Tor proxy at %s is not available: %v", proxyAddr, err)
		return false
	}
	conn.Close()
	return true
}

// tryConnectToTorAddress attempts to connect to a Tor address with retries and alternative ports
func tryConnectToTorAddress(addr string, maxRetries int) (net.Conn, error) {
	// Enable detailed debug logging if TOR_DEBUG is set
	debugMode := os.Getenv("TOR_DEBUG") == "true"
	if debugMode {
		log.Printf("crawler: DEBUG - attempting to connect to Tor address %s with max retries %d", addr, maxRetries)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid address format: %v", err)
	}

	// Try with the original port first
	ports := []string{port}

	// If environment variable is set to try alternative ports
	if os.Getenv("TRY_ALTERNATIVE_TOR_PORTS") == "true" {
		log.Printf("crawler: TRY_ALTERNATIVE_TOR_PORTS=true, will try ports: %v", alternativeTorPorts)
		ports = append(ports, alternativeTorPorts...)
	}

	// Check if Tor proxy is available
	if !isTorProxyAvailable(defaultTorProxy) {
		return nil, fmt.Errorf("Tor SOCKS5 proxy at %s is not available", defaultTorProxy)
	}

	// Try alternative Tor proxy ports if the default fails
	alternativeProxies := []string{"127.0.0.1:9150"} // Tor Browser often uses 9150
	proxyPorts := []string{defaultTorProxy}

	if os.Getenv("TRY_ALTERNATIVE_TOR_PROXIES") == "true" {
		log.Printf("crawler: TRY_ALTERNATIVE_TOR_PROXIES=true, will try proxy ports: %v", alternativeProxies)
		proxyPorts = append(proxyPorts, alternativeProxies...)
	}

	var lastErr error
	for _, proxyAddr := range proxyPorts {
		for _, p := range ports {
			torAddr := net.JoinHostPort(host, p)
			log.Printf("crawler: attempting to connect to Tor address %s via SOCKS5 proxy %s", torAddr, proxyAddr)

			for retry := 0; retry < maxRetries; retry++ {
				// Create a dialer with timeout
				dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
				if err != nil {
					lastErr = fmt.Errorf("failed to create SOCKS5 dialer with proxy %s: %v", proxyAddr, err)
					log.Printf("crawler: %v", lastErr)
					break // Try next proxy
				}

				// Try to dial with the SOCKS5 dialer
				conn, err := dialer.Dial("tcp", torAddr)
				if err != nil {
					lastErr = err
					log.Printf("crawler: retry %d/%d - failed to connect to %s via %s: %v",
						retry+1, maxRetries, torAddr, proxyAddr, err)

					// Check if this is a connection refused error, which means the port is closed
					if strings.Contains(err.Error(), "connection refused") {
						log.Printf("crawler: port %s is closed on %s, skipping retries", p, host)
						break // Skip retries for this port
					}

					time.Sleep(time.Duration(retry+1) * time.Second) // Exponential backoff
					continue
				}

				log.Printf("crawler: successfully connected to Tor address %s on port %s via proxy %s", host, p, proxyAddr)
				return conn, nil
			}
		}
	}

	return nil, fmt.Errorf("failed to connect to Tor address after all retries: %v", lastErr)
}

// Crawler discovers reachable Bitcoin peers by performing lightweight handshakes
// (version/verack) and requesting their addr list. Discovered peers are stored in
// the database and re-queued for crawling on subsequent passes.
//
// This is a *very* small-scale crawler: one worker goroutine, a bounded queue,
// and a map to deduplicate. It can be expanded with more concurrency, dns seed
// fetching, Tor/I2P support, etc.
//
// Usage:
//   pc := peercrawler.New(dbConn, []string{"seed.bitcoin.sipa.be:8333"})
//   go pc.Start(ctx)
//
// Schema expected in db:
//   CREATE TABLE peers (

type Crawler struct {
	db       *gorm.DB
	queue    chan string
	queued   map[string]struct{}
	seen     map[string]time.Time
	mu       sync.Mutex
	shutdown chan struct{}
	wg       sync.WaitGroup
	workers  int
	seeds    []string
}

func NewCrawler(db *gorm.DB, seeds []string) *Crawler {
	numWorkersStr := os.Getenv("PEER_CRAWLER_WORKERS")
	numWorkers, err := strconv.Atoi(numWorkersStr)
	if err != nil || numWorkers <= 0 {
		numWorkers = 3 // default workers
	}

	// Check if Tor proxy address is overridden by environment variable
	if envTorProxy := os.Getenv("TOR_PROXY"); envTorProxy != "" {
		defaultTorProxy = envTorProxy
		log.Printf("crawler: Using custom Tor proxy address: %s", defaultTorProxy)
	}

	// Check if Tor proxy is available
	torAvailable := isTorProxyAvailable(defaultTorProxy)
	if torAvailable {
		log.Printf("crawler: Tor SOCKS5 proxy is available at %s", defaultTorProxy)
	} else {
		log.Printf("crawler: WARNING: Tor SOCKS5 proxy is NOT available at %s. Tor connections will fail!", defaultTorProxy)
	}

	return &Crawler{
		db:       db,
		queue:    make(chan string, 10000),
		queued:   make(map[string]struct{}),
		seen:     make(map[string]time.Time),
		shutdown: make(chan struct{}),
		workers:  numWorkers,
		seeds:    seeds,
	}
}

func (c *Crawler) Start(ctx context.Context) {
	log.Printf("crawler: starting up with %d workers", c.workers)

	// Check for debug mode
	if os.Getenv("TOR_DEBUG") == "true" {
		log.Printf("crawler: TOR_DEBUG=true, enabling detailed Tor connection logging")
	}

	// Check Tor proxy availability
	if isTorProxyAvailable(defaultTorProxy) {
		log.Printf("crawler: Tor proxy is available at %s", defaultTorProxy)
	} else {
		log.Printf("crawler: WARNING: Tor proxy not available at %s, will not be able to connect to .onion addresses", defaultTorProxy)
		// Try alternative proxy
		if isTorProxyAvailable("127.0.0.1:9150") {
			log.Printf("crawler: Found alternative Tor proxy at 127.0.0.1:9150")
		}
	}

	// Add seed peers to the queue
	if len(c.seeds) > 0 {
		log.Printf("crawler: adding %d seed peers to the queue", len(c.seeds))
		
		// Count seeds by network type
		networkCounts := make(map[string]int)
		
		for _, addr := range c.seeds {
			networkType := getNetworkType(addr)
			networkCounts[networkType]++
			
			// If it's a Tor address, log it specially
			if networkType == "tor" {
				log.Printf("crawler: adding Tor seed peer: %s", addr)
			}
			c.push(addr)
		}
		
		// Log network type counts
		for networkType, count := range networkCounts {
			log.Printf("crawler: added %d %s seed peers", count, networkType)
		}
	}
	c.wg.Add(c.workers)
	for i := 0; i < c.workers; i++ {
		go c.worker(ctx, i+1)
	}

	c.wg.Wait()
	log.Println("crawler: all workers shut down")
}

func (c *Crawler) worker(ctx context.Context, workerID int) {
	defer c.wg.Done()
	log.Printf("crawler: worker %d started", workerID)

	for {
		select {
		case <-ctx.Done():
			log.Printf("crawler: worker %d stopping due to context done", workerID)
			return
		case <-c.shutdown:
			log.Printf("crawler: worker %d stopping due to shutdown signal", workerID)
			return
		case addr := <-c.queue:
			c.mu.Lock()
			delete(c.queued, addr)
			c.mu.Unlock()

			log.Printf("crawler: worker %d connecting to %s (queue=%d)", workerID, addr, len(c.queue))
			c.handlePeer(addr)
		}
	}
}

func (c *Crawler) Stop() {
	log.Println("crawler: sending shutdown signal")
	close(c.shutdown)
}

func (c *Crawler) push(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.queued[addr]; exists {
		return
	}
	if time.Since(c.seen[addr]).Minutes() < crawlInterval.Minutes() {
		return
	}

	select {
	case c.queue <- addr:
		c.queued[addr] = struct{}{}
	default:
		log.Printf("crawler: queue is full, dropping %s", addr)
	}
}

func (c *Crawler) recordPeerFailure(address string) {
	now := time.Now()
	err := c.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "address"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"failed_attempts": gorm.Expr("peers.failed_attempts + 1"),
			"last_failure":    &now,
			"updated_at":      now,
		}),
	}).Create(&db.Peer{
		Address:        address,
		Network:        getNetworkType(address),
		FailedAttempts: 1,
		LastFailure:    &now,
	}).Error

	if err != nil {
		log.Printf("crawler: db error recording failure for %s: %v", address, err)
	}
}

func (c *Crawler) handlePeer(addr string) {
	c.mu.Lock()
	c.seen[addr] = time.Now()
	c.mu.Unlock()

	var conn net.Conn
	var err error
	networkType := getNetworkType(addr)

	if networkType == "tor" {
		log.Printf("crawler: attempting to connect to Tor address %s with 5 retries", addr)
		conn, err = tryConnectToTorAddress(addr, 5) // Increased to 5 retries
		if err != nil {
			log.Printf("crawler: failed to connect to Tor address %s after retries: %v", addr, err)

			// Check if Tor is actually running
			if !isTorProxyAvailable(defaultTorProxy) && !isTorProxyAvailable("127.0.0.1:9150") {
				log.Printf("crawler: ERROR - No Tor proxy available at common ports. Make sure Tor is running!")
			}

			c.recordPeerFailure(addr)
			return
		}
		log.Printf("crawler: successfully connected to Tor address %s", addr)
	} else {
		conn, err = net.DialTimeout("tcp", addr, peerTimeout)
	}

	if err != nil {
		log.Printf("crawler: failed to dial %s: %v", addr, err)
		c.recordPeerFailure(addr)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(peerTimeout))

	// Use a lower protocol version for initial handshake to maximize compatibility
	// Bitcoin Core uses 31402 as the minimum supported version
	initialProtocolVersion := wire.ProtocolVersion
	if networkType == "tor" {
		// Use a more conservative version for Tor connections
		initialProtocolVersion = 70001 // Bitcoin protocol v0.10.0
		log.Printf("crawler: using conservative protocol version %d for Tor connection to %s", initialProtocolVersion, addr)
	}

	// Create version message
	ver := wire.NewMsgVersion(&wire.NetAddress{Timestamp: time.Now()}, &wire.NetAddress{Timestamp: time.Now()}, rand.Uint64(), 0)
	_ = ver.AddUserAgent("peerCrawler", "0.1")

	// Send version message with appropriate protocol version
	if err := wire.WriteMessage(conn, ver, initialProtocolVersion, wire.MainNet); err != nil {
		log.Printf("crawler: failed to send version to %s: %v", addr, err)
		c.recordPeerFailure(addr)
		return
	}

	var peerVersion *wire.MsgVersion
	var gotVerack bool
	handshakeAttempts := 0
	maxHandshakeAttempts := 10

	// Handshake loop
	for (peerVersion == nil || !gotVerack) && handshakeAttempts < maxHandshakeAttempts {
		handshakeAttempts++

		// Read with timeout
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		msg, _, err := wire.ReadMessage(conn, initialProtocolVersion, wire.MainNet)
		if err != nil {
			// Handle specific error cases
			if me, ok := err.(*wire.MessageError); ok {
				if strings.Contains(me.Error(), "unknown message") {
					log.Printf("crawler: received ignorable message during handshake with %s: %v", addr, err)
					continue
				} else if strings.Contains(me.Error(), "message payload is too large") {
					// This could be an RPC port instead of P2P port, or a protocol mismatch
					log.Printf("crawler: oversized message from %s - likely wrong port or protocol mismatch: %v", addr, err)
					c.recordPeerFailure(addr)
					return
				}
			}

			// Handle timeout errors
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("crawler: handshake timed out with %s after %d attempts", addr, handshakeAttempts)
				c.recordPeerFailure(addr)
				return
			}

			// General error case
			log.Printf("crawler: handshake read failed with %s: %v", addr, err)
			c.recordPeerFailure(addr)
			return
		}

		switch m := msg.(type) {
		case *wire.MsgVersion:
			peerVersion = m
			log.Printf("crawler: received version %d from %s, user agent: %s",
				m.ProtocolVersion, addr, m.UserAgent)

			// Send verack
			verack := wire.NewMsgVerAck()
			if err := wire.WriteMessage(conn, verack, initialProtocolVersion, wire.MainNet); err != nil {
				log.Printf("crawler: failed to send verack to %s: %v", addr, err)
				c.recordPeerFailure(addr)
				return
			}
		case *wire.MsgVerAck:
			gotVerack = true
			log.Printf("crawler: received verack from %s", addr)
		default:
			log.Printf("crawler: received unexpected message '%s' during handshake with %s", m.Command(), addr)
		}
	}

	if peerVersion == nil {
		log.Printf("crawler: failed to receive version from %s after %d attempts", addr, handshakeAttempts)
		c.recordPeerFailure(addr)
		return
	}

	if !gotVerack {
		log.Printf("crawler: failed to receive verack from %s after %d attempts", addr, handshakeAttempts)
		c.recordPeerFailure(addr)
		return
	}

	// Successfully completed handshake
	log.Printf("crawler: completed handshake with %s (network: %s)", addr, networkType)
	c.savePeer(addr, networkType, uint64(peerVersion.Services), time.Now(), peerVersion.UserAgent, peerVersion.LastBlock, int(peerVersion.ProtocolVersion))

	// Now use the peer's protocol version for subsequent communication
	negotiatedVersion := uint32(peerVersion.ProtocolVersion)
	if negotiatedVersion > wire.ProtocolVersion {
		negotiatedVersion = wire.ProtocolVersion
	}

	// Request addresses
	getAddr := wire.NewMsgGetAddr()
	if err := wire.WriteMessage(conn, getAddr, negotiatedVersion, wire.MainNet); err != nil {
		log.Printf("crawler: failed to send getaddr to %s: %v", addr, err)
		return
	}

	// Set a longer timeout for addr responses
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	addrReceived := false
	for !addrReceived {
		msg, _, err := wire.ReadMessage(conn, negotiatedVersion, wire.MainNet)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("crawler: timed out waiting for addr response from %s", addr)
				return
			}
			log.Printf("crawler: error reading message from %s: %v", addr, err)
			return
		}

		switch m := msg.(type) {
		case *wire.MsgAddr:
			log.Printf("crawler: received %d addresses from %s", len(m.AddrList), addr)
			c.ingestAddrMsg(m)
			addrReceived = true
		case *wire.MsgAddrV2:
			log.Printf("crawler: received %d addrv2 addresses from %s", len(m.AddrList), addr)
			c.ingestAddrV2Msg(m)
			addrReceived = true
		default:
			log.Printf("crawler: received non-addr message '%s' from %s", m.Command(), addr)
		}
	}
}

func (c *Crawler) ingestAddrMsg(m *wire.MsgAddr) {
	for _, na := range m.AddrList {
		if na.IP.IsUnspecified() || na.IP.IsLoopback() {
			continue
		}
		addrStr := net.JoinHostPort(na.IP.String(), strconv.Itoa(int(na.Port)))
		c.push(addrStr)
		c.savePeer(addrStr, getNetworkType(addrStr), uint64(na.Services), na.Timestamp, "", 0, 0)
	}
}

func (c *Crawler) ingestAddrV2Msg(m *wire.MsgAddrV2) {
	var torCount int
	var otherCount int

	for _, na := range m.AddrList {
		var addrStr, network string

		if na.IsTorV3() {
			network = "tor"
			addrStr = net.JoinHostPort(na.Addr.String(), strconv.Itoa(int(na.Port)))
			log.Printf("crawler: discovered Tor V3 address: %s", addrStr)
			torCount++
		} else if legacy := na.ToLegacy(); legacy != nil {
			if legacy.IP.IsUnspecified() || legacy.IP.IsLoopback() {
				continue
			}
			addrStr = net.JoinHostPort(legacy.IP.String(), strconv.Itoa(int(legacy.Port)))
			network = getNetworkType(addrStr) // Fallback to string matching for clearnet/i2p
			otherCount++
		} else {
			continue // Skip unsupported network types
		}

		// Prioritize Tor addresses in the queue
		if network == "tor" {
			// Double-check that this is actually a .onion address
			if strings.HasSuffix(addrStr, ".onion:"+strconv.Itoa(int(na.Port))) {
				log.Printf("crawler: queueing Tor address: %s", addrStr)
				c.push(addrStr)
				c.savePeer(addrStr, network, uint64(na.Services), time.Unix(na.Timestamp.Unix(), 0), "", 0, 0)
			} else {
				log.Printf("crawler: skipping invalid Tor address format: %s", addrStr)
			}
		} else {
			c.push(addrStr)
			c.savePeer(addrStr, network, uint64(na.Services), time.Unix(na.Timestamp.Unix(), 0), "", 0, 0)
		}
	}

	if torCount > 0 {
		log.Printf("crawler: processed %d Tor addresses and %d other addresses from AddrV2 message", torCount, otherCount)
	}
}

func (c *Crawler) savePeer(address, network string, services uint64, lastSeen time.Time, ua string, height int32, protoVer int) {
	isFullHandshake := ua != "" || height != 0 || protoVer != 0

	p := db.Peer{
		Address:         address,
		Network:         network,
		Services:        int64(services),
		LastSeen:        lastSeen,
		UserAgent:       ua,
		StartHeight:     int(height),
		ProtocolVersion: protoVer,
	}

	if isFullHandshake {
		updates := map[string]interface{}{
			"network":          p.Network,
			"services":         p.Services,
			"last_seen":        p.LastSeen,
			"user_agent":       p.UserAgent,
			"start_height":     p.StartHeight,
			"protocol_version": p.ProtocolVersion,
			"updated_at":       time.Now(),
			"failed_attempts":  0,
			"last_failure":     nil,
		}
		if err := c.db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "address"}},
			DoUpdates: clause.Assignments(updates),
		}).Create(&p).Error; err != nil {
			log.Printf("crawler: db error saving full handshake for %s: %v", address, err)
		} else {
			log.Printf("crawler: saved peer (full handshake) %s (%s)", address, network)
		}
	} else {
		result := c.db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "address"}},
			DoNothing: true,
		}).Create(&p)
		if result.Error != nil {
			log.Printf("crawler: db error saving partial peer %s: %v", address, result.Error)
		} else if result.RowsAffected > 0 {
			log.Printf("crawler: saved new peer (partial info) %s (%s)", address, network)
		}
	}
}

func getNetworkType(addr string) string {
	// Extract host part from address if it contains a port
	host := addr
	if hostPort, _, err := net.SplitHostPort(addr); err == nil {
		host = hostPort
	}

	// Remove IPv6 brackets if present
	host = strings.Trim(host, "[]")

	// Check for Tor addresses (.onion)
	if strings.HasSuffix(host, ".onion") {
		log.Printf("crawler: detected Tor address: %s", addr)
		return "tor"
	}

	// Check for I2P addresses (.b32.i2p)
	if strings.HasSuffix(host, ".b32.i2p") {
		log.Printf("crawler: detected I2P address: %s", addr)
		return "i2p"
	}

	// Check for CJDNS addresses (FC prefix in IPv6)
	if ip := net.ParseIP(host); ip != nil && len(ip) == net.IPv6len && ip[0] == 0xFC {
		log.Printf("crawler: detected CJDNS address: %s", addr)
		return "cjdns"
	}

	// Check if it's an IPv6 address
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		log.Printf("crawler: detected IPv6 address: %s", addr)
		return "ipv6"
	}

	// Check if it's an IPv4 address
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		log.Printf("crawler: detected IPv4 address: %s", addr)
		return "ipv4"
	}

	// Otherwise it's a hostname
	return "clearnet"
}
