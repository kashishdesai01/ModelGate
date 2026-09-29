package main

import (
	"net/http"
	"testing"
)

func TestMockServerHasBoundedRequestTimeouts(t *testing.T) {
	server := newMockServer(":0", http.NotFoundHandler())
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 {
		t.Fatalf("server timeouts are incomplete: %+v", server)
	}
}
