//go:build darwin && cgo

package main

/*
#include <arpa/inet.h>
#include <dns_sd.h>
#include <errno.h>
#include <poll.h>
#include <stdlib.h>

typedef struct {
    DNSServiceRef connection;
    DNSServiceErrorType error;
    int addressReady;
    int serviceReady;
} BridgeDNS;

static void addressRegistered(DNSServiceRef ref, DNSRecordRef record,
                              DNSServiceFlags flags, DNSServiceErrorType error,
                              void *context) {
    BridgeDNS *dns = context;
    if (error) {
        dns->error = error;
    } else {
        dns->addressReady = 1;
    }
}

static void serviceRegistered(DNSServiceRef ref, DNSServiceFlags flags,
                              DNSServiceErrorType error, const char *name,
                              const char *type, const char *domain, void *context) {
    BridgeDNS *dns = context;
    if (error) {
        dns->error = error;
    } else if (flags & kDNSServiceFlagsAdd) {
        dns->serviceReady = 1;
    } else {
        dns->error = kDNSServiceErr_NameConflict;
    }
}

static DNSServiceErrorType registerAddress(BridgeDNS *dns, uint32_t index,
                                           const char *hostname, const char *ip) {
    struct in_addr address;
    if (inet_pton(AF_INET, ip, &address) != 1) {
        return kDNSServiceErr_BadParam;
    }
    DNSServiceErrorType error = DNSServiceCreateConnection(&dns->connection);
    if (error) {
        return error;
    }
    DNSRecordRef record = NULL;
    return DNSServiceRegisterRecord(dns->connection, &record, kDNSServiceFlagsUnique,
        index, hostname, kDNSServiceType_A, kDNSServiceClass_IN,
        sizeof(address), &address, 120, addressRegistered, dns);
}

static DNSServiceErrorType registerService(BridgeDNS *dns, uint32_t index,
                                           const char *hostname, const char *name,
                                           const char *type, uint16_t port,
                                           uint16_t txtSize, const void *txt) {
    DNSServiceRef service = dns->connection;
    return DNSServiceRegister(&service,
        kDNSServiceFlagsShareConnection | kDNSServiceFlagsNoAutoRename,
        index, name, type, "local.", hostname, htons(port), txtSize, txt,
        serviceRegistered, dns);
}

static int pollBonjour(BridgeDNS *dns, int milliseconds) {
    struct pollfd descriptor = { DNSServiceRefSockFD(dns->connection), POLLIN, 0 };
    if (descriptor.fd < 0) {
        return kDNSServiceErr_BadReference;
    }
    int result = poll(&descriptor, 1, milliseconds);
    if (result < 0) {
        return errno == EINTR ? 0 : -1;
    }
    if (result == 0) {
        return 0;
    }
    if (!(descriptor.revents & POLLIN)) {
        return kDNSServiceErr_ServiceNotRunning;
    }
    DNSServiceErrorType error = DNSServiceProcessResult(dns->connection);
    return error ? error : dns->error;
}

static void freeBonjour(BridgeDNS *dns) {
    if (dns->connection) {
        DNSServiceRefDeallocate(dns->connection);
    }
    free(dns);
}
*/
import "C"

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"
)

type advertisement struct {
	Hostname string
	Errors   <-chan error
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func (a *advertisement) Close() {
	a.stopOnce.Do(func() { close(a.stop) })
	<-a.done
}

func advertise(iface *net.Interface, ip net.IP, port int, name string, fields map[string]string, universal bool, secure bool) (*advertisement, error) {
	if iface == nil || iface.Index <= 0 || ip.To4() == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("Bonjour needs an active Wi-Fi interface with an IPv4 address")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid Bonjour port %d", port)
	}
	if len(name) < 1 || len(name) > 63 || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 {
		return nil, fmt.Errorf("Bonjour printer name must contain 1 to 63 bytes of UTF-8 text")
	}
	txt, err := packBonjourTXT(fields)
	if err != nil {
		return nil, err
	}
	serviceType := "_ipp._tcp,_print"
	if secure {
		serviceType = "_ipps._tcp,_print"
	}
	if universal {
		serviceType += ",_universal"
	}
	localHost, _ := os.Hostname()
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", localHost, ip, name, port)))
	hostname := fmt.Sprintf("print-bridge-%x.local", hash[:8])
	// The default system hostname can also publish AAAA records. Give this
	// IPv4 listener a dedicated name with exactly one Wi-Fi address.
	cHost, cIP, cName, cType := C.CString(hostname+"."), C.CString(ip.String()), C.CString(name), C.CString(serviceType)
	cTXT := C.CBytes(txt)
	defer C.free(unsafe.Pointer(cHost))
	defer C.free(unsafe.Pointer(cIP))
	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cType))
	defer C.free(cTXT)

	dns := (*C.BridgeDNS)(C.calloc(1, C.sizeof_BridgeDNS))
	if dns == nil {
		return nil, fmt.Errorf("allocate Bonjour registration")
	}
	registered := false
	defer func() {
		if !registered {
			C.freeBonjour(dns)
		}
	}()
	index := C.uint32_t(iface.Index)
	if result := C.registerAddress(dns, index, cHost, cIP); result != 0 {
		return nil, bonjourError("register Wi-Fi address", int(result))
	}
	if err := awaitBonjour(dns, "register Wi-Fi address", &dns.addressReady); err != nil {
		return nil, err
	}
	if result := C.registerService(dns, index, cHost, cName, cType, C.uint16_t(port), C.uint16_t(len(txt)), cTXT); result != 0 {
		return nil, bonjourError("register printer service", int(result))
	}
	if err := awaitBonjour(dns, "register printer service", &dns.serviceReady); err != nil {
		return nil, err
	}

	errors := make(chan error, 1)
	a := &advertisement{
		Hostname: hostname,
		Errors:   errors,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	registered = true
	go a.monitor(dns, errors)
	return a, nil
}

func packBonjourTXT(fields map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seen := make(map[string]bool, len(fields))
	var txt []byte
	for _, key := range keys {
		if key == "" || strings.Contains(key, "=") || strings.IndexFunc(key, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
			return nil, fmt.Errorf("invalid Bonjour TXT key %q", key)
		}
		lowerKey := strings.ToLower(key)
		if seen[lowerKey] {
			return nil, fmt.Errorf("duplicate Bonjour TXT key %q", key)
		}
		seen[lowerKey] = true
		field := key + "=" + fields[key]
		if len(field) > 255 {
			return nil, fmt.Errorf("Bonjour TXT field %q exceeds 255 bytes", key)
		}
		txt = append(txt, byte(len(field)))
		txt = append(txt, field...)
	}
	if len(txt) == 0 {
		txt = append(txt, 0)
	}
	if len(txt) > 1300 {
		return nil, fmt.Errorf("Bonjour TXT record exceeds 1300 bytes")
	}
	return txt, nil
}

func awaitBonjour(dns *C.BridgeDNS, action string, ready *C.int) error {
	deadline := time.Now().Add(6 * time.Second)
	for *ready == 0 && time.Now().Before(deadline) {
		if err := processBonjour(dns, action); err != nil {
			return err
		}
	}
	if *ready == 0 {
		return fmt.Errorf("Bonjour could not %s before the registration timed out", action)
	}
	return nil
}

func (a *advertisement) monitor(dns *C.BridgeDNS, errors chan<- error) {
	defer close(a.done)
	defer close(errors)
	defer C.freeBonjour(dns)
	for {
		select {
		case <-a.stop:
			return
		default:
		}
		if err := processBonjour(dns, "keep printer advertised"); err != nil {
			errors <- err
			return
		}
	}
}

func processBonjour(dns *C.BridgeDNS, action string) error {
	result, err := C.pollBonjour(dns, 250)
	if result == -1 {
		return fmt.Errorf("Bonjour could not %s: %w", action, err)
	}
	if result != 0 {
		return bonjourError(action, int(result))
	}
	return nil
}

func bonjourError(action string, code int) error {
	switch code {
	case C.kDNSServiceErr_NameConflict:
		return fmt.Errorf("Bonjour could not %s: name already in use", action)
	case C.kDNSServiceErr_PolicyDenied, C.kDNSServiceErr_NoAuth:
		return fmt.Errorf("Bonjour could not %s: local network permission denied (%d)", action, code)
	default:
		return fmt.Errorf("Bonjour could not %s: DNS-SD error %d", action, code)
	}
}
