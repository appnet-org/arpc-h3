package transport

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/appnet-org/arpc-h3/pkg/packet"
	"github.com/appnet-org/arpc-h3/pkg/transport/balancer"
	"github.com/appnet-org/arpc/pkg/logging"
	"github.com/quic-go/quic-go/http3"
	"go.uber.org/zap"
)

// GenerateRPCID creates a unique RPC ID
func GenerateRPCID() uint64 {
	return uint64(time.Now().UnixNano())
}

type HTTP3Transport struct {
	server       *http3.Server
	client       *http.Client
	handler      http.Handler
	address      string
	reassembler  *DataReassembler
	resolver     *balancer.Resolver
	isServer     bool
	handlerMutex sync.RWMutex
	streams      map[uint64]*streamContext
	streamMutex  sync.RWMutex
}

type streamContext struct {
	dataChan   chan []byte
	addr       *net.UDPAddr
	rpcID      uint64
	packetType packet.PacketTypeID
	errChan    chan error
}

func NewHTTP3Transport(address string) (*HTTP3Transport, error) {
	return NewHTTP3TransportWithBalancer(address, balancer.DefaultResolver())
}

// NewHTTP3TransportWithBalancer creates a new HTTP/3 transport with a custom balancer
func NewHTTP3TransportWithBalancer(address string, resolver *balancer.Resolver) (*HTTP3Transport, error) {
	// Create TLS config
	tlsConfig, err := generateServerTLSConfig()
	if err != nil {
		return nil, err
	}

	transport := &HTTP3Transport{
		address:     address,
		reassembler: NewDataReassembler(),
		resolver:    resolver,
		isServer:    true,
		streams:     make(map[uint64]*streamContext),
	}

	// Create HTTP/3 server
	mux := http.NewServeMux()
	transport.handler = mux
	transport.server = &http3.Server{
		Addr:      address,
		Handler:   mux,
		TLSConfig: tlsConfig,
	}

	return transport, nil
}

// NewHTTP3ClientTransport creates an HTTP/3 transport for client use
func NewHTTP3ClientTransport() (*HTTP3Transport, error) {
	// Create HTTP/3 client
	client := &http.Client{
		Transport: &http3.RoundTripper{
			TLSClientConfig: generateClientTLSConfig(),
		},
	}

	transport := &HTTP3Transport{
		client:      client,
		reassembler: NewDataReassembler(),
		resolver:    balancer.DefaultResolver(),
		isServer:    false,
		streams:     make(map[uint64]*streamContext),
	}

	return transport, nil
}

// NewHTTP3TransportForStream creates an HTTP/3 transport for a server stream
func NewHTTP3TransportForStream(resolver *balancer.Resolver) *HTTP3Transport {
	return &HTTP3Transport{
		reassembler: NewDataReassembler(),
		resolver:    resolver,
		isServer:    true,
		streams:     make(map[uint64]*streamContext),
	}
}

// SetHandler sets the HTTP handler for the transport (server only)
func (t *HTTP3Transport) SetHandler(handler http.HandlerFunc) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	t.handlerMutex.Lock()
	defer t.handlerMutex.Unlock()
	t.handler = mux
	if t.server != nil {
		t.server.Handler = mux
	}
}

// Send sends data over HTTP/3 (client mode)
func (t *HTTP3Transport) Send(addr string, rpcID uint64, data []byte, packetTypeID packet.PacketTypeID) error {
	if t.isServer {
		return fmt.Errorf("Send not supported in server mode, use HTTP response writer directly")
	}

	// Ensure URL is properly formatted
	if addr == "" {
		return fmt.Errorf("address cannot be empty")
	}

	// Add https:// prefix if not present
	url := addr
	if len(url) < 8 || url[:8] != "https://" {
		url = "https://" + url
	}

	// For HTTP/3, IP/Port fields are not used (HTTP handles routing)
	// Use zero values as placeholders
	var dstIP [4]byte
	var dstPort uint16
	var srcIP [4]byte
	var srcPort uint16

	// Fragment the data into multiple packets if needed
	packets, err := t.reassembler.FragmentData(data, rpcID, packetTypeID, dstIP, dstPort, srcIP, srcPort)
	if err != nil {
		return err
	}

	// Serialize all packets into a single request body
	var requestData bytes.Buffer
	for _, pkt := range packets {
		var packetData []byte
		switch p := pkt.(type) {
		case *packet.DataPacket:
			packetData, err = packet.SerializeDataPacket(p)
		case *packet.ErrorPacket:
			packetData, err = packet.SerializeErrorPacket(p)
		default:
			return fmt.Errorf("unknown packet type: %T", pkt)
		}

		if err != nil {
			return fmt.Errorf("failed to serialize packet: %w", err)
		}

		// Write packet length first (4 bytes) for framing
		packetLen := uint32(len(packetData))
		lenBuf := make([]byte, 4)
		binary.LittleEndian.PutUint32(lenBuf, packetLen)
		requestData.Write(lenBuf)
		requestData.Write(packetData)
	}

	// Create HTTP/3 request
	req, err := http.NewRequest("POST", url, &requestData)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-RPC-ID", fmt.Sprintf("%d", rpcID))

	// Send request
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Store response for receive
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	// Store response in stream context
	t.streamMutex.Lock()
	ctx := &streamContext{
		dataChan:   make(chan []byte, 1),
		addr:       nil, // Client doesn't need addr
		rpcID:      rpcID,
		packetType: packetTypeID,
		errChan:    make(chan error, 1),
	}
	ctx.dataChan <- respBody
	t.streams[rpcID] = ctx
	t.streamMutex.Unlock()

	return nil
}

// Receive takes a buffer size as input, reads data from HTTP/3 responses, and returns
// the following information when receiving the complete data for an RPC message:
// * complete data for a message (if no message is complete, it will return nil)
// * original source address from connection (for responses)
// * RPC id
// * packet type
// * error
func (t *HTTP3Transport) Receive(bufferSize int) ([]byte, *net.UDPAddr, uint64, packet.PacketTypeID, error) {
	// For server, we need to check if we have any completed streams
	if t.isServer {
		t.streamMutex.RLock()
		for rpcID, ctx := range t.streams {
			select {
			case data := <-ctx.dataChan:
				t.streamMutex.RUnlock()
				// Process the received data
				return t.ProcessReceivedData(data, ctx.addr, rpcID, ctx.packetType, bufferSize)
			default:
				// No data available for this stream, continue
			}
		}
		t.streamMutex.RUnlock()
		// No data available
		return nil, nil, 0, packet.PacketTypeUnknown, nil
	} else {
		// Client: check for response data
		t.streamMutex.RLock()
		for rpcID, ctx := range t.streams {
			select {
			case data := <-ctx.dataChan:
				t.streamMutex.RUnlock()
				// Process the received data
				return t.ProcessReceivedData(data, nil, rpcID, ctx.packetType, bufferSize)
			default:
				// No data available for this stream, continue
			}
		}
		t.streamMutex.RUnlock()
		// No data available
		return nil, nil, 0, packet.PacketTypeUnknown, nil
	}
}

// ProcessReceivedData processes received data and handles fragmentation
func (t *HTTP3Transport) ProcessReceivedData(data []byte, addr *net.UDPAddr, rpcID uint64, packetTypeID packet.PacketTypeID, bufferSize int) ([]byte, *net.UDPAddr, uint64, packet.PacketTypeID, error) {
	// Read packets from the data (they are framed with length prefixes)
	offset := 0

	for offset < len(data) {
		if offset+4 > len(data) {
			return nil, nil, 0, packet.PacketTypeUnknown, fmt.Errorf("data too short for packet length")
		}

		// Read packet length
		packetLen := binary.LittleEndian.Uint32(data[offset : offset+4])
		offset += 4

		if offset+int(packetLen) > len(data) {
			return nil, nil, 0, packet.PacketTypeUnknown, fmt.Errorf("packet length %d exceeds remaining data", packetLen)
		}

		if packetLen > uint32(bufferSize) {
			return nil, nil, 0, packet.PacketTypeUnknown, fmt.Errorf("packet length %d exceeds buffer size %d", packetLen, bufferSize)
		}

		// Read packet data
		packetData := data[offset : offset+int(packetLen)]
		offset += int(packetLen)

		// Deserialize packet
		pkt, pktType, err := packet.DeserializePacket(packetData)
		if err != nil {
			return nil, nil, 0, packet.PacketTypeUnknown, err
		}

		// Handle different packet types
		switch p := pkt.(type) {
		case *packet.DataPacket:
			// Process fragment through reassembly layer
			message, _, reassembledRPCID, isComplete := t.reassembler.ProcessFragment(p, addr)
			if isComplete {
				// Clean up stream context
				t.streamMutex.Lock()
				delete(t.streams, rpcID)
				t.streamMutex.Unlock()
				return message, addr, reassembledRPCID, pktType, nil
			}
			// Still waiting for more fragments, but we've processed this one
			// Continue processing more packets
		case *packet.ErrorPacket:
			// Clean up stream context
			t.streamMutex.Lock()
			delete(t.streams, rpcID)
			t.streamMutex.Unlock()
			return []byte(p.ErrorMsg), addr, p.RPCID, pktType, nil
		default:
			logging.Debug("Unknown packet type", zap.Uint8("packetTypeID", uint8(packetTypeID)))
			return nil, nil, 0, packetTypeID, nil
		}
	}

	// If we get here, we processed packets but didn't get a complete message
	return nil, nil, 0, packetTypeID, nil
}

// ListenAndServe starts the HTTP/3 server (server mode)
func (t *HTTP3Transport) ListenAndServe() error {
	if !t.isServer || t.server == nil {
		return fmt.Errorf("ListenAndServe can only be called in server mode")
	}

	logging.Info("Starting HTTP/3 server", zap.String("addr", t.address))
	return t.server.ListenAndServe()
}

// Close closes the transport
func (t *HTTP3Transport) Close() error {
	t.streamMutex.Lock()
	defer t.streamMutex.Unlock()

	var err error
	if t.server != nil {
		err = t.server.Close()
		t.server = nil
	}

	if t.client != nil {
		t.client.CloseIdleConnections()
		t.client = nil
	}

	// Close all streams
	for _, ctx := range t.streams {
		close(ctx.dataChan)
		close(ctx.errChan)
	}
	t.streams = make(map[uint64]*streamContext)

	return err
}

// LocalAddr returns the local address of the transport
func (t *HTTP3Transport) LocalAddr() *net.UDPAddr {
	if t.server != nil {
		addr, err := net.ResolveUDPAddr("udp", t.address)
		if err != nil {
			return nil
		}
		return addr
	}
	return nil
}

// GetResolver returns the resolver for this transport
func (t *HTTP3Transport) GetResolver() *balancer.Resolver {
	return t.resolver
}

// GetClient returns the HTTP client for this transport (client mode)
func (t *HTTP3Transport) GetClient() *http.Client {
	return t.client
}

// GetReassembler returns the data reassembler
func (t *HTTP3Transport) GetReassembler() *DataReassembler {
	return t.reassembler
}

// generateClientTLSConfig generates a basic TLS config for HTTP/3
func generateClientTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h3"},
	}
}

// generateServerTLSConfig generates a self-signed TLS certificate for HTTP/3
func generateServerTLSConfig() (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"ARPC-H3"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"h3"},
	}, nil
}
