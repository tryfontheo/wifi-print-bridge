//go:build darwin && cgo

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"testing"
	"time"
)

func TestDiscoveryPrinterUsesAdvertisedPortAndResource(t *testing.T) {
	for _, tc := range []struct{ resource, want string }{
		{"", "ipp://192.0.2.25:618/"},
		{"ipp/print", "ipp://192.0.2.25:618/ipp/print"},
		{"/queues/Office%20Printer", "ipp://192.0.2.25:618/queues/Office%20Printer"},
		{"//wifi-host.local/ipp", ""},
		{"ipp/print?host=x", ""},
		{"ipp/%xyz", ""},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			got := discoveryPrinterURI(net.IPv4(192, 0, 2, 25), 618, tc.resource)
			if got != tc.want {
				t.Fatalf("printer URI = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestDiscoveryKeepsDistinctInterfacesAndFiltersLocalAddresses(t *testing.T) {
	first := discoveredPrinter{Name: "Alpha", Address: "192.0.2.25", URI: "ipp://192.0.2.25:631/ipp/print", Interface: "en8"}
	second := first
	second.Interface = "en9"
	duplicate := first
	duplicate.Name, duplicate.AirPrint = "Beta", true
	bridge := discoveredPrinter{Name: "The Mac", Address: "192.168.0.11", URI: "ipp://192.168.0.11:8631/ipp/print", Interface: "en8"}

	got := organizeDiscovery([]discoveredPrinter{duplicate, bridge, second, first}, map[string]bool{"192.168.0.11": true})
	first.AirPrint = true
	if !slices.Equal(got, []discoveredPrinter{first, second}) {
		t.Fatalf("printers = %#v", got)
	}
}

func TestBonjourDiscoveryOnLoopback(t *testing.T) {
	if os.Getenv("PRINT_BRIDGE_TEST_BONJOUR") != "1" {
		t.Skip("set PRINT_BRIDGE_TEST_BONJOUR=1 to exercise the local mDNS responder")
	}
	iface, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("Print Bridge loopback test %d", os.Getpid())
	ip := net.IPv4(127, 0, 0, 1)
	const port = 18637
	ad, err := advertise(iface, ip, port, name,
		map[string]string{"rp": "queues/test%20location", "pdl": "image/jpeg,image/urf", "URF": "W8"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ad.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := discoverPrintersOn(ctx, *iface)
	if err != nil {
		t.Fatal(err)
	}
	want := discoveredPrinter{
		Name: name, Address: "127.0.0.1", URI: "ipp://127.0.0.1:18637/queues/test%20location",
		Interface: "lo0", AirPrint: true,
	}
	if !slices.Contains(got, want) {
		t.Fatalf("did not find %#v in %#v", want, got)
	}
}
