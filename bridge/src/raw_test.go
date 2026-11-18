package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

const rawTestTimeout = 3 * time.Second

func rawTestListener(t *testing.T) *net.TCPListener {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func rawTestConnect(t *testing.T, ln *net.TCPListener) *net.TCPConn {
	t.Helper()
	dialer := net.Dialer{Timeout: rawTestTimeout}
	conn, err := dialer.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	tcp := conn.(*net.TCPConn)
	_ = tcp.SetDeadline(time.Now().Add(rawTestTimeout))
	return tcp
}

func rawTestServe(t *testing.T, allowed net.IP, dial windowsPrinterDial) (*net.TCPListener, func()) {
	t.Helper()
	ln := rawTestListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := serveWindowsConnections(ctx, ln, allowed.Equal, dial); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(rawTestTimeout):
			t.Error("Windows listener did not stop after cancellation")
		}
	}
	t.Cleanup(stop)
	return ln, stop
}

func TestWindowsRelayWaitsForUploadEOFBeforeReturningStatus(t *testing.T) {
	job := bytes.Repeat([]byte("print payload "), 1<<13)
	status := []byte("job complete")
	backend := rawTestListener(t)
	_ = backend.SetDeadline(time.Now().Add(rawTestTimeout))
	backendDone := make(chan error, 1)
	go func() {
		conn, err := backend.AcceptTCP()
		if err != nil {
			backendDone <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(rawTestTimeout))
		data, err := io.ReadAll(conn)
		if err != nil || !bytes.Equal(job, data) {
			backendDone <- fmt.Errorf("job mismatch: read %d bytes of %d, error %v", len(data), len(job), err)
			return
		}
		_, err = conn.Write(status)
		backendDone <- err
	}()

	front, _ := rawTestServe(t, net.IPv4(127, 0, 0, 1), func(ctx context.Context) (*net.TCPConn, error) {
		dialer := net.Dialer{Timeout: rawTestTimeout}
		conn, err := dialer.DialContext(ctx, "tcp4", backend.Addr().String())
		if err != nil {
			return nil, err
		}
		return conn.(*net.TCPConn), nil
	})
	client := rawTestConnect(t, front)
	if _, err := client.Write(job); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(client)
	if err != nil || !bytes.Equal(status, data) {
		t.Fatalf("status mismatch: %q, %v", data, err)
	}
	if err := <-backendDone; err != nil {
		t.Fatal(err)
	}
}

func TestWindowsRelayDialsOnlyForAllowedClientsWithData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed net.IP
		payload string
	}{
		{"wrong source IP with payload", net.IPv4(127, 0, 0, 2), "ignored"},
		{"allowed source IP without payload", net.IPv4(127, 0, 0, 1), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var connections atomic.Int32
			front, stop := rawTestServe(t, tc.allowed, func(context.Context) (*net.TCPConn, error) {
				connections.Add(1)
				return nil, errors.New("unexpected connection to printer")
			})
			client := rawTestConnect(t, front)
			if tc.payload != "" {
				_, _ = io.WriteString(client, tc.payload)
			}
			_ = client.CloseWrite()
			if _, err := io.ReadAll(client); err != nil {
				var timeout net.Error
				if errors.As(err, &timeout) && timeout.Timeout() {
					t.Fatalf("frontend did not close the connection: %v", err)
				}
			}
			stop()
			if n := connections.Load(); n != 0 {
				t.Fatalf("printer dialed %d times", n)
			}
		})
	}
}

func TestWindowsWiFiSubnetAllowsMultipleClients(t *testing.T) {
	_, subnet, err := net.ParseCIDR("192.0.2.0/25")
	if err != nil {
		t.Fatal(err)
	}
	lan := settings{windowsNetwork: subnet}
	for _, address := range []string{"192.0.2.3", "192.0.2.126"} {
		if !lan.allowsWindowsClient(net.ParseIP(address)) {
			t.Errorf("blocked client %s from the Wi-Fi subnet", address)
		}
	}
	for _, address := range []string{"192.0.2.129", "198.51.100.10", "2001:db8::1"} {
		if lan.allowsWindowsClient(net.ParseIP(address)) {
			t.Errorf("allowed client %s outside the Wi-Fi subnet", address)
		}
	}
}
