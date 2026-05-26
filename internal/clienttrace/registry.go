// Package clienttrace maintains a process-wide map from centrifuge client ID
// to the connection's context.Context.  The context typically carries the
// OTel SpanContext extracted from the WebSocket upgrade request, which lets
// the zerolog → OTel bridge attach trace_id / span_id to library-originated
// log lines (e.g. "client command error") that have no other ctx available.
package clienttrace

import (
	"context"
	"sync"
)

var registry sync.Map // map[string]context.Context

// Store associates ctx with clientID. Safe to call concurrently.
func Store(clientID string, ctx context.Context) {
	if clientID == "" || ctx == nil {
		return
	}
	registry.Store(clientID, ctx)
}

// Get returns the ctx previously stored for clientID, or nil if absent.
func Get(clientID string) context.Context {
	if clientID == "" {
		return nil
	}
	v, ok := registry.Load(clientID)
	if !ok {
		return nil
	}
	return v.(context.Context)
}

// Delete removes the entry for clientID. Safe to call on missing keys.
func Delete(clientID string) {
	if clientID == "" {
		return
	}
	registry.Delete(clientID)
}
