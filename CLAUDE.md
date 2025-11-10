# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ion-sfu is a Go implementation of a WebRTC Selective Forwarding Unit (SFU). It provides video routing services that allow WebRTC sessions to scale efficiently by forwarding media streams between peers without transcoding.

## Common Commands

### Testing

```bash
# Run all tests (excludes cmd and examples packages)
make test

# Tests run with race detection, 240s timeout, and generate cover.out
```

### Building

```bash
# Build gRPC signaling server
make build_grpc
# Output: bin/sfu-grpc

# Build JSON-RPC signaling server
make build_jsonrpc
# Output: bin/sfu-json-rpc

# Build all-RPC signaling server
make build_allrpc
# Output: bin/sfu-allrpc
```

### Linting

```bash
# Generate code and run golangci-lint (as per CI)
go generate ./...
golangci-lint run
```

### Running Examples

```bash
# Run echo test with docker-compose
docker-compose -f examples/echotest-jsonrpc/docker-compose.yaml up
# Then open http://localhost:8000/
```

## Architecture

### Core Components

**pkg/sfu/** - Core SFU implementation

- `sfu.go` - Main SFU struct, manages sessions and WebRTC transport configuration
- `session.go` - Session interface and SessionLocal implementation. Sessions contain peers and handle media routing between them
- `peer.go` - Peer connections (publisher/subscriber model)
- `publisher.go` - Publishing peer that sends media
- `subscriber.go` - Subscribing peer that receives media
- `router.go` - Routes media streams between publishers and subscribers within a session
- `receiver.go` - Handles incoming RTP streams from publishers
- `downtrack.go` - Manages outgoing RTP streams to subscribers
- `datachannel.go` - DataChannel middleware support
- `audioobserver.go` - Audio level detection for "who is speaking" feature

**pkg/buffer/** - RTP buffer management

- Handles packet buffering, reordering, NACK handling
- Factory pattern for buffer creation

**cmd/signal/** - Signaling servers

- `grpc/` - gRPC-based signaling (service-to-service)
- `json-rpc/` - JSON-RPC signaling (browser-friendly)
- `allrpc/` - Combined signaling server

### Key Concepts

**Pub/Sub Model**: ion-sfu uses a publisher/subscriber pattern where each peer has two peer connections:

- Publisher connection: sends media to the SFU
- Subscriber connection: receives media from the SFU

**Session**: A Session groups multiple peers together. All publishers in a session are automatically forwarded to all subscribers in that session. Sessions are identified by string IDs.

**Router**: Routes media from Receivers (incoming) to Downtracks (outgoing) based on session membership and subscription state.

**Receiver/Downtrack**:

- Receiver handles incoming RTP from a publisher
- Downtrack handles outgoing RTP to a subscriber (supports simulcast layer selection, bandwidth adaptation)

### Configuration

Configuration is loaded from `config.toml` using viper. Key settings include:

- WebRTC ICE configuration (STUN/TURN servers, port ranges, single port mode)
- Router configuration (audio level thresholds, simulcast settings)
- Buffer factory settings

### Testing Notes

- Tests exclude `cmd/` and `examples/` packages
- Race detection is enabled
- Coverage output: `cover.out`
- Timeout: 240 seconds

### WebRTC Details

- Uses Pion WebRTC library (github.com/pion/webrtc/v3)
- Supports unified plan semantics
- Implements TWCC, REMB, RR/SR for congestion control
- Audio level indication via RFC6464
