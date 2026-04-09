package main

import (
	"net"
	"testing"
	"time"
)

func TestWaitForTransferCompletion(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		serverConn.Close()
	}()

	if err := waitForTransferCompletion(clientConn, 200*time.Millisecond); err != nil {
		t.Fatalf("expected EOF after server close, got: %v", err)
	}

	<-done
}
