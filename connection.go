package monitor

import (
	"time"

	"github.com/techquest-tech/gin-shared/pkg/core"
)

// ConnectionAction is the kind of a connection event.
type ConnectionAction string

const (
	ConnectionOpen  ConnectionAction = "open"
	ConnectionClose ConnectionAction = "close"
)

// ConnectionDetails is a connection-level event: an HTTP/2 transport came up or
// went away.
//
// It deliberately does not reuse TracingDetails. The granularity differs: request
// tracing answers "how did this call go", a connection event answers "when did
// this transport come up, when did it die, how long did it live, and was it doing
// any work before it died". One connection can carry zero RPCs - which is exactly
// the "idle connection silently died" case - and folding those rows into
// monitor_tracing would pollute request counts, latency percentiles and every
// per-operation aggregate. Hence a separate stream and a separate table.
//
// Identity fields (Client / Device / SN) come from RPC metadata, so a connection
// that never issued an RPC has them empty. Peer is the TCP peer, which behind a
// gateway is the ingress rather than the device.
type ConnectionDetails struct {
	// Action is open or close.
	Action ConnectionAction
	// HappenedAt is when this event happened (equal to StartedAt for open).
	HappenedAt time.Time
	// ConnID is a producer-local counter that ties the open/close pair together.
	ConnID string
	// StartedAt is when the transport was established.
	StartedAt time.Time
	// Duration is the connection lifetime, set on close.
	Duration time.Duration
	// RPCs is how many RPCs ran on this connection. On close it separates
	// "died mid-work" from "sat idle and then died".
	RPCs int64

	// Peer is the TCP peer address (host:port). Behind Traefik this is the
	// ingress, so it cannot identify a device.
	Peer string
	// LocalAddr is the local address.
	LocalAddr string
	// ClientIP is the forwarded client IP. Empty when the gateway injected
	// nothing; a client can send anything here, so it is not trusted.
	ClientIP string

	// Client / Device / SN map to the client, device-id and device-sn metadata.
	Client string
	Device string
	SN     string

	AppName    string
	AppVersion string
	Tenant     string

	// Reason is the disconnect reason, filled only when the producer knows it.
	// The server cannot observe it, so it stays empty there and the exact cause
	// lives in the transport log line "Closing: <reason>".
	Reason string
}

// ConnectionAdaptor is the global adaptor for connection-level events.
// Defaults to the in-process chan implementation; a process can swap it for a
// redis-streaming adaptor (see messaging.NewRedisAdaptor) before services start.
var ConnectionAdaptor core.Adaptor[ConnectionDetails] = core.NewChanAdaptor[ConnectionDetails](10000)

// PushConnection is a nil-safe push: a connection event is never worth panicking
// over, and producers may run in processes where the monitor stack is not wired.
func PushConnection(cd ConnectionDetails) {
	if ConnectionAdaptor == nil {
		return
	}
	ConnectionAdaptor.Push(cd)
}
