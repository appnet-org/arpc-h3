# arpc-h3

aRPC (Async RPC) implementation using HTTP/3 as the transport layer.

## Overview

arpc-h3 is a high-performance RPC framework built on top of HTTP/3, which leverages QUIC as its underlying transport protocol. This implementation provides a modern, standards-based approach to RPC communication with the benefits of HTTP/3's multiplexing, low latency, and built-in encryption.

## Features

- **HTTP/3 Transport**: Uses HTTP/3 for reliable, multiplexed communication
- **QUIC-based**: Leverages QUIC's performance benefits (reduced latency, improved congestion control)
- **TLS Encryption**: Built-in security with TLS 1.3
- **Multiple Serializers**: Support for Protocol Buffers, Symphony, and Cap'n Proto
- **Load Balancing**: Built-in support for various load balancing strategies
- **Simplified Fragmentation**: HTTP/3 handles fragmentation internally

## Architecture

### Transport Layer
- **HTTP/3 Server**: Uses `quic-go/http3` for server-side HTTP/3 support
- **HTTP/3 Client**: Uses `http3.RoundTripper` for client connections
- **Protocol**: ALPN protocol "h3" for HTTP/3 negotiation

### Wire Format
Requests and responses use a simple binary protocol:
```
[PacketTypeID (1B)][RPCID (8B)][DataLength (4B)][Data]
```

Where:
- `PacketTypeID`: Packet type (Data=1, Error=3, Unknown=0)
- `RPCID`: Unique identifier for the RPC call
- `DataLength`: Length of the data payload
- `Data`: Framed RPC data containing service name, method name, and serialized payload

### Request Framing
```
[ServiceLen (2B)][Service][MethodLen (2B)][Method][Payload]
```

## Getting Started

### Installation

```bash
go get github.com/appnet-org/arpc-h3
```

### Server Example

```go
import (
    "github.com/appnet-org/arpc-h3/pkg/rpc"
    "github.com/appnet-org/arpc-h3/pkg/serializer"
)

// Create server
server, err := rpc.NewServer(":11000", &serializer.ProtobufSerializer{})
if err != nil {
    log.Fatal(err)
}

// Register service
server.RegisterService(serviceDesc, serviceImpl)

// Start server (blocks)
server.Start()
```

### Client Example

```go
import (
    "context"
    "github.com/appnet-org/arpc-h3/pkg/rpc"
    "github.com/appnet-org/arpc-h3/pkg/serializer"
)

// Create client
client, err := rpc.NewClient(&serializer.ProtobufSerializer{}, "localhost:11000")
if err != nil {
    log.Fatal(err)
}

// Make RPC call
var response MyResponse
err = client.Call(context.Background(), "MyService", "MyMethod", &request, &response)
if err != nil {
    log.Fatal(err)
}
```

## Serialization

arpc-h3 supports multiple serialization formats:

- **Protocol Buffers**: `serializer.ProtobufSerializer{}`
- **Symphony**: `serializer.SymphonySerializer{}`
- **Cap'n Proto**: `serializer.CapnpSerializer{}`

## Load Balancing

The framework includes built-in load balancing support:

- **Round Robin**: Distributes requests evenly across backends
- **Random**: Randomly selects a backend for each request

Example:
```go
import "github.com/appnet-org/arpc-h3/pkg/transport/balancer"

resolver := balancer.NewResolver(balancer.RoundRobinBalancer())
transport, err := transport.NewHTTP3TransportWithBalancer(":11000", resolver)
```

## Differences from Raw QUIC Implementation

Previous versions of this library (arpc-quic) used raw QUIC streams. The HTTP/3 version provides:

1. **Standardization**: Uses the standard HTTP/3 protocol instead of custom QUIC streams
2. **Simplified Code**: HTTP/3 handles many low-level details (framing, flow control)
3. **Better Tooling**: Standard HTTP tools can be used for debugging and monitoring
4. **Interoperability**: Easier integration with existing HTTP infrastructure
5. **Automatic Fragmentation**: HTTP/3 handles packet fragmentation internally

## Performance Considerations

- **Payload Size**: Default max payload is 1MB (configurable)
- **Connection Pooling**: HTTP/3 client automatically pools connections
- **Multiplexing**: Multiple RPCs can be in flight over a single HTTP/3 connection
- **TLS Overhead**: TLS 1.3 handshake is optimized with 0-RTT support

## Security

- **TLS 1.3**: All connections are encrypted with TLS 1.3
- **Self-signed Certificates**: Development mode uses self-signed certificates
- **Production**: Configure proper TLS certificates for production use

## Benchmarks

A key-value store benchmark is included in `benchmark/kv-store/`:

```bash
cd benchmark/kv-store
go build -o kvstore ./kvstore
go build -o frontend ./frontend

# Run server
./kvstore

# Run frontend
./frontend
```

## Contributing

Contributions are welcome! Please ensure all tests pass and code follows Go best practices.

## License

[License information]

## Acknowledgments

Built on top of:
- [quic-go](https://github.com/quic-go/quic-go) - QUIC and HTTP/3 implementation
- [arpc](https://github.com/appnet-org/arpc) - Base aRPC framework
