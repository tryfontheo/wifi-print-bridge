package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const (
	windowsPrintPort     = "9100"
	windowsPrintSessions = 4
	windowsJobTimeout    = 5 * time.Minute
	windowsInputTimeout  = 30 * time.Second
	windowsStatusTimeout = 20 * time.Second
)

type windowsPrinting struct {
	Errors <-chan error
	cancel context.CancelFunc
	done   <-chan struct{}
}

func (w *windowsPrinting) Close() {
	w.cancel()
	<-w.done
}

type windowsPrinterDial func(context.Context) (*net.TCPConn, error)

func (s settings) windowsEnabled() bool {
	return s.windowsNetwork != nil || s.windowsClient != nil
}

func (s settings) windowsScope() string {
	if s.windowsNetwork != nil {
		return "Wi-Fi subnet " + s.windowsNetwork.String()
	}
	return s.windowsClient.String()
}

func (s settings) allowsWindowsClient(ip net.IP) bool {
	if s.windowsNetwork != nil {
		return s.windowsNetwork.Contains(ip)
	}
	return s.windowsClient != nil && s.windowsClient.Equal(ip)
}

func windowsPrinterConnection(s settings) windowsPrinterDial {
	target := net.JoinHostPort(s.backend.Hostname(), windowsPrintPort)
	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: s.source},
		Control:   bindInterface(s.ethernet),
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context) (*net.TCPConn, error) {
		conn, err := dialPrinter(ctx, dialer, target)
		if err != nil {
			return nil, err
		}
		return conn.(*net.TCPConn), nil
	}
}

// Probe only the TCP handshake. Printers interpret bytes on this port as jobs.
func probeWindowsPrinter(ctx context.Context, s settings) error {
	if !s.windowsEnabled() {
		return errors.New("Windows RAW printing is disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := windowsPrinterConnection(s)(ctx)
	if err != nil {
		return fmt.Errorf("probe printer's Ethernet TCP port %s: %w", windowsPrintPort, err)
	}
	_ = conn.Close()
	return nil
}

func startWindowsPrinting(ctx context.Context, s settings) (*windowsPrinting, error) {
	if !s.windowsEnabled() {
		return nil, errors.New("Windows RAW printing is disabled")
	}
	addr := net.JoinHostPort(s.wifiIP.String(), windowsPrintPort)
	config := net.ListenConfig{Control: bindInterface(s.wifi)}
	listener, err := config.Listen(ctx, "tcp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for Windows printing on Wi-Fi %s: %w", addr, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	failures := make(chan error, 1)
	done := make(chan struct{})
	windows := &windowsPrinting{Errors: failures, cancel: cancel, done: done}
	go func() {
		defer close(done)
		if err := serveWindowsConnections(ctx, listener.(*net.TCPListener), s.allowsWindowsClient, windowsPrinterConnection(s)); err != nil {
			failures <- err
		}
	}()
	return windows, nil
}

func superviseWindowsPrinting(ctx context.Context, s settings) {
	for ctx.Err() == nil {
		if err := probeWindowsPrinter(ctx, s); err == nil {
			windows, listenErr := startWindowsPrinting(ctx, s)
			if listenErr == nil {
				log.Printf("Windows RAW printing listening on %s:%s for %s.", s.wifiIP, windowsPrintPort, s.windowsScope())
				select {
				case <-ctx.Done():
				case err := <-windows.Errors:
					log.Printf("Windows RAW printing stopped: %v", err)
				}
				windows.Close()
			} else {
				log.Printf("Windows RAW printing could not start: %v", listenErr)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func serveWindowsConnections(ctx context.Context, listener *net.TCPListener, allowed func(net.IP) bool, dial windowsPrinterDial) error {
	ctx, cancel := context.WithCancel(ctx)
	slots := make(chan struct{}, windowsPrintSessions)
	var active sync.WaitGroup
	stopListening := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer func() {
		cancel()
		stopListening()
		_ = listener.Close()
		active.Wait()
	}()

	for {
		client, err := listener.AcceptTCP()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept Windows print connection: %w", err)
		}
		if !allowed(client.RemoteAddr().(*net.TCPAddr).IP) {
			_ = client.Close()
			continue
		}
		select {
		case slots <- struct{}{}:
			active.Add(1)
			go func() {
				defer active.Done()
				defer func() { <-slots }()
				if err := relayWindowsJob(ctx, client, dial); err != nil {
					log.Printf("Windows printing: %v", err)
				}
			}()
		default:
			_ = client.Close()
		}
	}
}

func relayWindowsJob(ctx context.Context, client *net.TCPConn, dial windowsPrinterDial) error {
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, windowsJobTimeout)
	defer cancel()
	stopClient := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClient()

	// An allowed workstation can check the port without opening a job on the
	// printer. Once any data reaches the printer the job is never replayed.
	_ = client.SetReadDeadline(time.Now().Add(windowsInputTimeout))
	job := bufio.NewReader(client)
	if _, err := job.Peek(1); err != nil {
		return nil
	}

	printer, err := dial(ctx)
	if err != nil {
		return fmt.Errorf("connect to printer: %w", err)
	}
	defer printer.Close()
	stopPrinter := context.AfterFunc(ctx, func() { _ = printer.Close() })
	defer stopPrinter()

	deadline, _ := ctx.Deadline()
	_ = client.SetDeadline(deadline)
	_ = printer.SetDeadline(deadline)

	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		_, _ = io.Copy(client, printer)
		_ = client.CloseWrite()
	}()
	defer func() {
		_ = printer.Close()
		_ = client.Close()
		<-statusDone
	}()

	if _, err := io.Copy(printer, job); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("send print job: %w", err)
	}
	if err := printer.CloseWrite(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("finish print job: %w", err)
	}

	// Some printers keep the status side open after finishing the upload.
	// Let a final status reply through without retaining a slot indefinitely.
	statusDeadline := time.Now().Add(windowsStatusTimeout)
	if deadline.Before(statusDeadline) {
		statusDeadline = deadline
	}
	_ = printer.SetReadDeadline(statusDeadline)
	_ = client.SetWriteDeadline(statusDeadline)
	select {
	case <-ctx.Done():
	case <-statusDone:
	}
	return nil
}
