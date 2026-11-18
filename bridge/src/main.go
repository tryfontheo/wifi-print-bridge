package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const printerPath = "/ipp/print"

type settings struct {
	wifi, ethernet *net.Interface
	wifiIP, source net.IP
	windowsClient  net.IP
	windowsNetwork *net.IPNet
	backend        *url.URL
	name           string
	port           int
}

func interfaceNetwork(iface *net.Interface, address net.IP) (*net.IPNet, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		ip, network, err := net.ParseCIDR(addr.String())
		if err == nil && address.Equal(ip) {
			return network, nil
		}
	}
	return nil, fmt.Errorf("cannot determine the Wi-Fi subnet for %s", address)
}

func interfaceAddress(iface *net.Interface, want string) (net.IP, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil && ip.To4() != nil && (want == "" || ip.Equal(net.ParseIP(want))) {
			return ip.To4(), nil
		}
	}
	if want != "" {
		return nil, fmt.Errorf("%s does not own IPv4 address %s", iface.Name, want)
	}
	return nil, fmt.Errorf("%s does not have an IPv4 address", iface.Name)
}

func parseSettings(args []string) (settings, error) {
	var s settings
	flags := flag.NewFlagSet("print-bridge", flag.ContinueOnError)
	wifi := flags.String("wifi-interface", "", "Wi-Fi interface")
	ethernet := flags.String("ethernet-interface", "", "Printer-side Ethernet interface")
	wifiIP := flags.String("wifi-ip", "", "Optional Wi-Fi listening IPv4 address")
	source := flags.String("ethernet-source", "", "Optional Ethernet source IPv4 address")
	windowsClient := flags.String("windows-client", "", "Optional Windows laptop IPv4 address for RAW printing")
	windowsLAN := flags.Bool("windows-lan", false, "Allow RAW printing from the Mac's Wi-Fi IPv4 subnet")
	backend := flags.String("backend", "", "Physical printer IPP URI")
	flags.StringVar(&s.name, "name", "Ethernet Printer", "Name shown on phones")
	flags.IntVar(&s.port, "port", 8631, "Wi-Fi listening TCP port")
	if err := flags.Parse(args); err != nil {
		return s, err
	}
	if flags.NArg() != 0 || *wifi == "" || *ethernet == "" ||
		s.port < 1024 || s.port > 65535 || strings.TrimSpace(s.name) == "" || len(s.name) > 63 ||
		strings.ContainsAny(s.name, "\x00\r\n") {
		return s, errors.New("provide both interfaces, a 1–63 byte printer name and a port from 1024 to 65535")
	}

	var err error
	if s.wifi, err = net.InterfaceByName(*wifi); err != nil {
		return s, fmt.Errorf("Wi-Fi interface %q: %w", *wifi, err)
	}
	if s.ethernet, err = net.InterfaceByName(*ethernet); err != nil {
		return s, fmt.Errorf("Ethernet interface %q: %w", *ethernet, err)
	}
	if s.wifi.Index == s.ethernet.Index && s.wifi.Flags&net.FlagLoopback == 0 {
		return s, errors.New("Wi-Fi and Ethernet must be separate interfaces")
	}
	if s.wifiIP, err = interfaceAddress(s.wifi, *wifiIP); err != nil {
		return s, err
	}
	if s.source, err = interfaceAddress(s.ethernet, *source); err != nil {
		return s, err
	}
	if *windowsLAN && *windowsClient != "" {
		return s, errors.New("choose either --windows-lan or --windows-client")
	}
	if *windowsLAN {
		s.windowsNetwork, err = interfaceNetwork(s.wifi, s.wifiIP)
		if err != nil {
			return s, err
		}
	}
	if *windowsClient != "" {
		s.windowsClient = net.ParseIP(*windowsClient).To4()
		if s.windowsClient == nil || !s.windowsClient.IsGlobalUnicast() ||
			s.windowsClient.Equal(s.wifiIP) || s.windowsClient.Equal(s.source) {
			return s, errors.New("enter the Windows laptop's separate Wi-Fi IPv4 address")
		}
	}
	if s.windowsEnabled() && s.port == 9100 {
		return s, errors.New("the IPP bridge and Windows print port must use different ports")
	}
	s.backend, err = url.Parse(*backend)
	if err != nil || s.backend.Scheme != "ipp" || s.backend.User != nil ||
		s.backend.RawQuery != "" || s.backend.ForceQuery || s.backend.Fragment != "" ||
		s.backend.Path == "" || net.ParseIP(s.backend.Hostname()).To4() == nil {
		return s, errors.New("provide the Ethernet printer's numeric IPv4 IPP URI")
	}
	if s.backend.Port() == "" {
		s.backend.Host = net.JoinHostPort(s.backend.Hostname(), "631")
	}
	backendIP := net.ParseIP(s.backend.Hostname())
	if !(backendIP.IsGlobalUnicast() || backendIP.IsLinkLocalUnicast() || backendIP.IsLoopback()) || (backendIP.IsLoopback() && s.ethernet.Flags&net.FlagLoopback == 0) ||
		(backendIP.Equal(s.source) || backendIP.Equal(s.wifiIP)) && !backendIP.IsLoopback() {
		return s, errors.New("backend must be the address of a separate Ethernet printer")
	}
	return s, nil
}

// IP_BOUND_IF is Darwin's per-socket IPv4 interface binding. The Ethernet
// source address is also bound so a directly wired printer can reply to it.
func bindInterface(iface *net.Interface) func(string, string, syscall.RawConn) error {
	return func(_, _ string, conn syscall.RawConn) error {
		var sockErr error
		err := conn.Control(func(fd uintptr) {
			sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, iface.Index)
		})
		if err != nil {
			return err
		}
		return sockErr
	}
}

func (s settings) client() *http.Client {
	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: s.source}, Control: bindInterface(s.ethernet),
		Timeout: 3 * time.Second, KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: 6 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
				return dialPrinter(ctx, dialer, address)
			},
			DisableCompression: true, IdleConnTimeout: 30 * time.Second,
			MaxConnsPerHost: 12, MaxIdleConnsPerHost: 4, ResponseHeaderTimeout: 90 * time.Second,
			MaxResponseHeaderBytes: 24 << 10,
		},
	}
}

func dialPrinter(ctx context.Context, dialer *net.Dialer, address string) (net.Conn, error) {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		conn, err := dialer.DialContext(ctx, "tcp4", address)
		var timeout net.Error
		if err == nil || attempt == attempts || !errors.As(err, &timeout) || !timeout.Timeout() {
			return conn, err
		}
		// A timed-out TCP handshake has not sent any HTTP or document bytes.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func run(ctx context.Context, command string, s settings) error {
	client := s.client()
	defer client.CloseIdleConnections()
	caps, err := probePrinter(ctx, client, s.backend)
	if err != nil {
		return fmt.Errorf("printer probe from %s (%s) to %s: %w", s.ethernet.Name, s.source, s.backend, err)
	}
	log.Printf("Printer: %s | AirPrint: %t | Windows IPP: %t | Formats: %s",
		caps.model, caps.airPrint, caps.windowsIPP, caps.fields["pdl"])
	if caps.legacy != nil {
		log.Print("AirPrint conversion: PostScript over IPP.")
	}
	if command == "probe" {
		rawReady := false
		if s.windowsEnabled() {
			if err := probeWindowsPrinter(ctx, s); err != nil {
				if s.windowsClient != nil {
					return fmt.Errorf("printer Windows TCP port 9100: %w", err)
				}
				log.Printf("Windows RAW printing unavailable: %v", err)
			} else {
				rawReady = true
				log.Printf("Windows RAW printing is available on %s:9100 for %s.", s.wifiIP, s.windowsScope())
			}
		}
		if caps.fields["pdl"] == "" && !rawReady {
			return errors.New("the printer did not report a supported print format")
		}
		log.Printf("Wi-Fi print address: http://%s%s", net.JoinHostPort(s.wifiIP.String(), fmt.Sprint(s.port)), printerPath)
		return nil
	}

	addr := net.JoinHostPort(s.wifiIP.String(), fmt.Sprint(s.port))
	listen := net.ListenConfig{Control: bindInterface(s.wifi)}
	listener, err := listen.Listen(ctx, "tcp4", addr)
	if err != nil {
		return fmt.Errorf("listen on Wi-Fi %s: %w", addr, err)
	}
	defer listener.Close()
	hostname := s.wifiIP.String()
	var advertisementErrors <-chan error
	if s.wifi.Flags&net.FlagLoopback == 0 {
		ad, err := advertise(s.wifi, s.wifiIP, s.port, s.name, caps.fields, caps.airPrint, false)
		if err != nil {
			return fmt.Errorf("advertise printer on Wi-Fi: %w", err)
		}
		defer ad.Close()
		hostname = ad.Hostname
		advertisementErrors = ad.Errors
	}
	front := url.URL{Scheme: "ipp", Host: net.JoinHostPort(hostname, fmt.Sprint(s.port)), Path: printerPath}
	proxy := &ippProxy{
		client: client, backend: s.backend, backendPrinter: caps.printerURI,
		front: front, wifiAddress: addr, slots: make(chan struct{}, 12),
		legacy: caps.legacy, conversionSlots: make(chan struct{}, 2),
	}
	rand.Read(proxy.jobKey[:])
	server := &http.Server{
		Handler: proxy, MaxHeaderBytes: 16 << 10, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 5 * time.Minute, WriteTimeout: 6 * time.Minute, IdleTimeout: 30 * time.Second,
	}
	defer server.Close()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	log.Printf("Listening at %s (Wi-Fi %s %s). Printer backend %s (%s %s).",
		front.String(), s.wifi.Name, s.wifiIP, s.backend, s.ethernet.Name, s.source)

	if s.windowsEnabled() {
		windowsCtx, cancelWindows := context.WithCancel(ctx)
		windowsDone := make(chan struct{})
		go func() {
			defer close(windowsDone)
			superviseWindowsPrinting(windowsCtx, s)
		}()
		defer func() {
			cancelWindows()
			<-windowsDone
		}()
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdown, done := context.WithTimeout(context.Background(), 6*time.Second)
			defer done()
			return server.Shutdown(shutdown)
		case err := <-advertisementErrors:
			return fmt.Errorf("Bonjour registration ended: %w", err)
		case err := <-serverErrors:
			return err
		case <-ticker.C:
			// Force launchd's supervisor to recreate the scoped sockets and
			// Bonjour address record after DHCP, adapter changes or wake.
			for _, endpoint := range []struct {
				iface *net.Interface
				ip    net.IP
			}{{s.wifi, s.wifiIP}, {s.ethernet, s.source}} {
				iface, err := net.InterfaceByName(endpoint.iface.Name)
				if err != nil || iface.Index != endpoint.iface.Index {
					return fmt.Errorf("interface %s changed", endpoint.iface.Name)
				}
				if _, err = interfaceAddress(iface, endpoint.ip.String()); err != nil {
					return err
				}
			}
			if s.windowsNetwork != nil {
				subnet, err := interfaceNetwork(s.wifi, s.wifiIP)
				if err != nil || subnet.String() != s.windowsNetwork.String() {
					return errors.New("the Wi-Fi subnet changed")
				}
			}
		}
	}
}

func probePrinter(ctx context.Context, client *http.Client, backend *url.URL) (capabilities, error) {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		probeCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
		caps, err := probe(probeCtx, client, backend)
		cancel()
		// A capability query is safe to retry. Print requests are never
		// replayed here because the printer might have accepted their data.
		var connectionError net.Error
		if err == nil || attempt == attempts || !errors.As(err, &connectionError) {
			return caps, err
		}
		log.Printf("Ethernet printer connection failed. Retrying the capability check (%d/%d).", attempt+1, attempts)
		select {
		case <-ctx.Done():
			return caps, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: print-bridge {discover|probe|serve} [options]")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch os.Args[1] {
	case "discover":
		if err := runDiscovery(ctx, os.Args[2:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Fatal(err)
		}
		return
	case "probe", "serve":
	default:
		fmt.Fprintln(os.Stderr, "Usage: print-bridge {discover|probe|serve} [options]")
		os.Exit(2)
	}
	s, err := parseSettings(os.Args[2:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	if err := run(ctx, os.Args[1], s); err != nil {
		log.Fatal(err)
	}
}
