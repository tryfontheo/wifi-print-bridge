//go:build darwin && cgo

package main

/*
#include <arpa/inet.h>
#include <dns_sd.h>
#include <errno.h>
#include <poll.h>
#include <stdlib.h>
#include <string.h>

enum { discoveryMaxServices = 64, discoveryMaxAddresses = 8 };

typedef struct {
    int present;
    unsigned char ipv4[4];
} PrinterAddress;

typedef struct PrinterBrowse PrinterBrowse;

typedef struct {
    PrinterBrowse *browse;
    DNSServiceRef resolve;
    DNSServiceRef query;
    char name[64];
    char hostname[kDNSServiceMaxDomainName];
    char resource[256];
    uint16_t port;
    int airPrint;
    PrinterAddress addresses[discoveryMaxAddresses];
} PrinterService;

struct PrinterBrowse {
    DNSServiceRef connection;
    uint32_t index;
    DNSServiceErrorType error;
    PrinterService services[discoveryMaxServices];
};

static void discoveryPermissionError(PrinterBrowse *browse, DNSServiceErrorType error) {
    if (error == kDNSServiceErr_PolicyDenied || error == kDNSServiceErr_NoAuth)
        browse->error = error;
}

static void printerAddressFound(DNSServiceRef ref, DNSServiceFlags flags,
                                 uint32_t index, DNSServiceErrorType error,
                                 const char *name, uint16_t type, uint16_t rrclass,
                                 uint16_t length, const void *record, uint32_t ttl, void *context) {
    PrinterService *service = context;
    if (error) {
        discoveryPermissionError(service->browse, error);
        return;
    }
    if (index != service->browse->index || type != kDNSServiceType_A ||
        rrclass != kDNSServiceClass_IN || length != 4)
        return;

    PrinterAddress *available = NULL;
    for (int i = 0; i < discoveryMaxAddresses; i++) {
        PrinterAddress *address = &service->addresses[i];
        if (!address->present) {
            if (!available)
                available = address;
        } else if (!memcmp(address->ipv4, record, sizeof(address->ipv4))) {
            address->present = (flags & kDNSServiceFlagsAdd) != 0;
            return;
        }
    }
    if (available && (flags & kDNSServiceFlagsAdd)) {
        memcpy(available->ipv4, record, sizeof(available->ipv4));
        available->present = 1;
    }
}

static int printerAdvertisesAirPrint(uint16_t length, const unsigned char *txt) {
    uint8_t urfLength, pdlLength;
    const char *urf = TXTRecordGetValuePtr(length, txt, "URF", &urfLength);
    const char *pdl = TXTRecordGetValuePtr(length, txt, "pdl", &pdlLength);
    if (!urf || !urfLength || (urfLength == 4 && !strncasecmp(urf, "none", 4)) || !pdl)
        return 0;
    while (pdlLength) {
        const char *comma = memchr(pdl, ',', pdlLength);
        size_t tokenLength = comma ? (size_t)(comma - pdl) : pdlLength;
        if (tokenLength == 9 && !strncasecmp(pdl, "image/urf", 9))
            return 1;
        if (!comma)
            break;
        pdlLength -= tokenLength + 1;
        pdl = comma + 1;
    }
    return 0;
}

static void printerServiceResolved(DNSServiceRef ref, DNSServiceFlags flags,
                                   uint32_t index, DNSServiceErrorType error,
                                   const char *name, const char *hostname, uint16_t port,
                                   uint16_t txtLength, const unsigned char *txt, void *context) {
    PrinterService *service = context;
    PrinterBrowse *browse = service->browse;
    if (error) {
        discoveryPermissionError(browse, error);
        return;
    }
    if (index != browse->index || !port)
        return;
    size_t hostLength = strlen(hostname);
    if (hostLength < 8 || strcasecmp(hostname + hostLength - 7, ".local."))
        return;

    uint8_t resourceLength = 0;
    const char *resource = TXTRecordGetValuePtr(txtLength, txt, "rp", &resourceLength);
    if (resource && memchr(resource, 0, resourceLength))
        return;
    if (resource)
        memcpy(service->resource, resource, resourceLength);
    service->resource[resource ? resourceLength : 0] = 0;
    service->port = ntohs(port);
    service->airPrint = printerAdvertisesAirPrint(txtLength, txt);

    if (service->query) {
        if (!strcasecmp(service->hostname, hostname))
            return;
        DNSServiceRefDeallocate(service->query);
        service->query = NULL;
        memset(service->addresses, 0, sizeof(service->addresses));
    }
    strlcpy(service->hostname, hostname, sizeof(service->hostname));
    DNSServiceRef query = browse->connection;
    error = DNSServiceQueryRecord(&query, kDNSServiceFlagsShareConnection | kDNSServiceFlagsForceMulticast,
        index, hostname, kDNSServiceType_A, kDNSServiceClass_IN, printerAddressFound, service);
    if (error)
        discoveryPermissionError(browse, error);
    else
        service->query = query;
}

static void printerServiceFound(DNSServiceRef ref, DNSServiceFlags flags,
                                uint32_t index, DNSServiceErrorType error,
                                const char *name, const char *type,
                                const char *domain, void *context) {
    PrinterBrowse *browse = context;
    if (error) {
        browse->error = error;
        return;
    }
    if (index != browse->index || strcasecmp(domain, "local."))
        return;

    PrinterService *available = NULL;
    for (int i = 0; i < discoveryMaxServices; i++) {
        PrinterService *service = &browse->services[i];
        if (!service->resolve) {
            if (!available)
                available = service;
        } else if (!strcasecmp(service->name, name)) {
            if (!(flags & kDNSServiceFlagsAdd)) {
                if (service->query)
                    DNSServiceRefDeallocate(service->query);
                DNSServiceRefDeallocate(service->resolve);
                memset(service, 0, sizeof(*service));
            }
            return;
        }
    }
    if (!available || !(flags & kDNSServiceFlagsAdd) || strlen(name) >= sizeof(available->name))
        return;

    available->browse = browse;
    strlcpy(available->name, name, sizeof(available->name));
    DNSServiceRef resolve = browse->connection;
    error = DNSServiceResolve(&resolve, kDNSServiceFlagsShareConnection | kDNSServiceFlagsForceMulticast,
        index, name, type, domain, printerServiceResolved, available);
    if (error) {
        discoveryPermissionError(browse, error);
        memset(available, 0, sizeof(*available));
    } else {
        available->resolve = resolve;
    }
}

static void closePrinterBrowse(PrinterBrowse *browse) {
    if (browse->connection)
        DNSServiceRefDeallocate(browse->connection);
    free(browse);
}

static PrinterBrowse *startPrinterBrowse(uint32_t index, DNSServiceErrorType *error) {
    PrinterBrowse *browse = calloc(1, sizeof(*browse));
    if (!browse) {
        *error = kDNSServiceErr_NoMemory;
        return NULL;
    }
    browse->index = index;
    *error = DNSServiceCreateConnection(&browse->connection);
    if (!*error) {
        DNSServiceRef ref = browse->connection;
        *error = DNSServiceBrowse(&ref, kDNSServiceFlagsShareConnection, index, "_ipp._tcp", "local.",
            printerServiceFound, browse);
    }
    if (*error) {
        closePrinterBrowse(browse);
        return NULL;
    }
    return browse;
}

static int pollPrinterBrowse(PrinterBrowse *browse, int ms) {
    struct pollfd descriptor = { DNSServiceRefSockFD(browse->connection), POLLIN, 0 };
    if (descriptor.fd < 0)
        return kDNSServiceErr_BadReference;
    int result = poll(&descriptor, 1, ms);
    if (result < 0)
        return errno == EINTR ? 0 : -1;
    if (!result)
        return 0;
    if (!(descriptor.revents & POLLIN))
        return kDNSServiceErr_ServiceNotRunning;
    DNSServiceErrorType error = DNSServiceProcessResult(browse->connection);
    return error ? error : browse->error;
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

type discoveredPrinter struct {
	Name      string `json:"name"`
	URI       string `json:"uri"`
	Address   string `json:"address"`
	Interface string `json:"interface"`
	AirPrint  bool   `json:"air_print"`
}

func runDiscovery(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("print-bridge discover", flag.ContinueOnError)
	wifi := flags.String("wifi-interface", "", "Wi-Fi interface, excluded from discovery")
	ethernet := flags.String("ethernet-interface", "", "Optional Ethernet interface to browse")
	timeout := flags.Duration("timeout", 6*time.Second, "Bonjour discovery timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !ethernetInterfaceName(*wifi) {
		return errors.New("discovery needs --wifi-interface enN")
	}
	if *timeout < time.Second || *timeout > 30*time.Second {
		return errors.New("discovery timeout must be between 1s and 30s")
	}
	if *ethernet != "" && (!ethernetInterfaceName(*ethernet) || *ethernet == *wifi) {
		return errors.New("choose an Ethernet interface separate from Wi-Fi")
	}
	if _, err := net.InterfaceByName(*wifi); err != nil {
		return fmt.Errorf("Wi-Fi interface %q: %w", *wifi, err)
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("list network interfaces: %w", err)
	}
	var candidates []net.Interface
	local := make(map[string]bool)
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		hasIPv4 := false
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil {
				local[ip.String()] = true
				hasIPv4 = true
			}
		}
		if hasIPv4 && iface.Flags&net.FlagUp != 0 && ethernetInterfaceName(iface.Name) &&
			iface.Name != *wifi && (*ethernet == "" || iface.Name == *ethernet) {
			candidates = append(candidates, iface)
		}
	}
	if len(candidates) == 0 {
		if *ethernet != "" {
			return fmt.Errorf("Ethernet interface %q is unavailable or has no IPv4 address", *ethernet)
		}
		return errors.New("no active Ethernet interface has an IPv4 address")
	}

	browseCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	type result struct {
		printers []discoveredPrinter
		err      error
	}
	results := make(chan result, len(candidates))
	for _, iface := range candidates {
		go func() {
			printers, err := discoverPrintersOn(browseCtx, iface)
			results <- result{printers, err}
		}()
	}
	printers := []discoveredPrinter{}
	var failures []error
	for range candidates {
		result := <-results
		if result.err != nil {
			failures = append(failures, result.err)
		}
		printers = append(printers, result.printers...)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(failures) == len(candidates) {
		return errors.Join(failures...)
	}
	return json.NewEncoder(os.Stdout).Encode(organizeDiscovery(printers, local))
}

func ethernetInterfaceName(name string) bool {
	digits, ok := strings.CutPrefix(name, "en")
	return ok && digits != "" && strings.IndexFunc(digits, func(r rune) bool {
		return r < '0' || r > '9'
	}) == -1
}

func discoverPrintersOn(ctx context.Context, iface net.Interface) ([]discoveredPrinter, error) {
	action := "browse for Ethernet printers on " + iface.Name
	var status C.DNSServiceErrorType
	browse := C.startPrinterBrowse(C.uint32_t(iface.Index), &status)
	if browse == nil {
		return nil, bonjourError(action, int(status))
	}
	defer C.closePrinterBrowse(browse)

	for ctx.Err() == nil {
		code, err := C.pollPrinterBrowse(browse, 100)
		if code == -1 {
			return nil, fmt.Errorf("Bonjour could not %s: %w", action, err)
		}
		if code != 0 {
			return nil, bonjourError(action, int(code))
		}
	}

	var printers []discoveredPrinter
	for i := range browse.services {
		service := &browse.services[i]
		if service.resolve == nil || service.port == 0 {
			continue
		}
		for _, address := range service.addresses {
			if address.present == 0 {
				continue
			}
			ip := net.IPv4(byte(address.ipv4[0]), byte(address.ipv4[1]), byte(address.ipv4[2]), byte(address.ipv4[3]))
			if !ip.IsGlobalUnicast() && !ip.IsLoopback() {
				continue
			}
			uri := discoveryPrinterURI(ip, int(service.port), C.GoString(&service.resource[0]))
			if uri != "" {
				printers = append(printers, discoveredPrinter{
					Name: C.GoString(&service.name[0]), URI: uri, Address: ip.String(),
					Interface: iface.Name, AirPrint: service.airPrint != 0,
				})
			}
		}
	}
	return printers, nil
}

func discoveryPrinterURI(ip net.IP, port int, resource string) string {
	path := "/" + strings.TrimPrefix(resource, "/")
	parsed, err := url.ParseRequestURI(path)
	if err != nil || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\r\n\x00") {
		return ""
	}
	return (&url.URL{
		Scheme: "ipp", Host: net.JoinHostPort(ip.String(), strconv.Itoa(port)),
		Path: parsed.Path, RawPath: parsed.RawPath,
	}).String()
}

func organizeDiscovery(printers []discoveredPrinter, local map[string]bool) []discoveredPrinter {
	slices.SortFunc(printers, func(a, b discoveredPrinter) int {
		if order := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); order != 0 {
			return order
		}
		if order := strings.Compare(a.URI, b.URI); order != 0 {
			return order
		}
		return strings.Compare(a.Interface, b.Interface)
	})
	results := make([]discoveredPrinter, 0, len(printers))
	seen := make(map[[2]string]int)
	for _, printer := range printers {
		if local[printer.Address] {
			continue
		}
		key := [2]string{printer.Interface, printer.URI}
		if index, ok := seen[key]; ok {
			results[index].AirPrint = results[index].AirPrint || printer.AirPrint
			continue
		}
		seen[key] = len(results)
		results = append(results, printer)
	}
	return results
}
