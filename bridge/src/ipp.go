package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/OpenPrinting/goipp"
)

const (
	maxAttributes  = 1 << 20
	maxJobBytes    = 256 << 20
	jobPathPrefix  = "/ipp/job/"
	postScriptMIME = "application/postscript"
)

var mobileOps = []goipp.Op{
	goipp.OpGetPrinterAttributes, goipp.OpValidateJob, goipp.OpPrintJob,
	goipp.OpCreateJob, goipp.OpSendDocument, goipp.OpCloseJob, goipp.OpGetJobAttributes,
	goipp.OpGetJobs, goipp.OpCancelJob,
}

var jobOps = []goipp.Op{
	goipp.OpSendDocument, goipp.OpCloseJob, goipp.OpGetJobAttributes, goipp.OpCancelJob,
}

type capabilities struct {
	printerURI           string
	model                string
	airPrint, windowsIPP bool
	fields               map[string]string
	legacy               *legacyFormats
}

type legacyFormats struct {
	convert   []string
	supported []string
	urf       []string
}

func legacyPostScriptFormats(formats []string, color bool) *legacyFormats {
	out := &legacyFormats{supported: slices.Clone(formats), urf: []string{"W8", "RS300"}}
	if color {
		out.urf = append(out.urf, "SRGB24")
	}
	for _, format := range []string{"image/urf", "application/pdf", "image/pwg-raster", "image/jpeg"} {
		if !slices.Contains(out.supported, format) {
			out.supported = append(out.supported, format)
			out.convert = append(out.convert, format)
		} else if format == "image/urf" {
			// Native URF without its capability record cannot be negotiated.
			out.convert = append(out.convert, format)
		}
	}
	return out
}

func stringsFrom(attrs goipp.Attributes, name string) []string {
	for _, a := range attrs {
		if a.Name == name {
			var values []string
			for _, v := range a.Values {
				if str, ok := v.V.(goipp.String); ok && str != "" {
					values = append(values, string(str))
				}
			}
			return values
		}
	}
	return nil
}

func first(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return values[0]
}

func truth(v bool) string {
	if v {
		return "T"
	}
	return "F"
}

func clipTXT(s string) string {
	// Leave room for the TXT key, and never split a UTF-8 character.
	if len(s) <= 225 {
		return s
	}
	for i := range s {
		if i > 220 {
			return s[:i]
		}
	}
	return s
}

func probe(ctx context.Context, client *http.Client, backend *url.URL) (capabilities, error) {
	var caps capabilities
	var response *goipp.Message
	for _, version := range []goipp.Version{goipp.DefaultVersion, goipp.MakeVersion(1, 1)} {
		req := goipp.NewRequest(version, goipp.OpGetPrinterAttributes, 1)
		req.Operation.Add(goipp.MakeAttr("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		req.Operation.Add(goipp.MakeAttr("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
		req.Operation.Add(goipp.MakeAttr("printer-uri", goipp.TagURI, goipp.String(backend.String())))
		req.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("all")))
		prefix, err := req.EncodeBytes()
		if err != nil {
			return caps, err
		}
		response, err = postIPP(ctx, client, backend, bytes.NewReader(prefix), int64(len(prefix)))
		if err != nil {
			return caps, err
		}
		if goipp.Status(response.Code) != goipp.StatusErrorVersionNotSupported {
			break
		}
	}
	if response.Code >= 0x0100 {
		return caps, fmt.Errorf("Get-Printer-Attributes returned %s", goipp.Status(response.Code))
	}

	printer := response.Printer
	caps.printerURI = backend.String()
	for _, value := range stringsFrom(printer, "printer-uri-supported") {
		u, err := url.Parse(value)
		if err == nil && u.Scheme == "ipp" && u.User == nil && u.Hostname() != "" &&
			u.EscapedPath() == backend.EscapedPath() && u.RawQuery == "" && u.Fragment == "" {
			caps.printerURI = value
			break
		}
	}
	auth := stringsFrom(printer, "uri-authentication-supported")
	if len(auth) > 0 && !slices.Contains(auth, "none") {
		return caps, errors.New("the printer requires client authentication")
	}
	formats := stringsFrom(printer, "document-format-supported")
	urf := slices.DeleteFunc(stringsFrom(printer, "urf-supported"), func(v string) bool {
		return strings.EqualFold(v, "none")
	})
	caps.airPrint = slices.Contains(formats, "image/urf") && len(urf) > 0
	color := slices.ContainsFunc(printer, func(a goipp.Attribute) bool {
		return a.Name == "color-supported" && hasValue(a.Values, goipp.Boolean(true))
	})
	if !caps.airPrint && slices.Contains(formats, postScriptMIME) && postScriptConverterAvailable() {
		caps.legacy = legacyPostScriptFormats(formats, color)
		formats, urf = caps.legacy.supported, caps.legacy.urf
		caps.airPrint = true
	}
	modernFormat := slices.Contains(formats, "application/pdf") || slices.Contains(formats, "image/pwg-raster") ||
		slices.Contains(formats, "application/PCLm") || slices.Contains(formats, "application/pclm")
	modernVersion := slices.ContainsFunc(stringsFrom(printer, "ipp-versions-supported"), func(version string) bool {
		return strings.HasPrefix(version, "2.")
	})
	caps.model = first(stringsFrom(printer, "printer-make-and-model"), "Ethernet Printer")
	for _, a := range printer {
		if a.Name != "operations-supported" {
			continue
		}
		multiRequestJobs := hasValue(a.Values, goipp.Integer(goipp.OpCreateJob)) &&
			hasValue(a.Values, goipp.Integer(goipp.OpSendDocument))
		caps.windowsIPP = modernFormat && modernVersion && multiRequestJobs
		if !hasValue(a.Values, goipp.Integer(goipp.OpPrintJob)) && !multiRequestJobs {
			return caps, errors.New("the printer did not report an IPP job submission operation")
		}
	}

	// Keep preferred mobile formats first in the TXT record.
	var advertised []string
	for _, format := range []string{
		"application/pdf", "image/urf", "image/pwg-raster", "application/PCLm", "application/pclm", "image/jpeg",
		"image/png", "application/postscript", "application/vnd.hp-PCL", "application/vnd.hp-PCLXL",
	} {
		if slices.Contains(formats, format) {
			advertised = append(advertised, format)
		}
	}
	duplex := slices.Contains(stringsFrom(printer, "sides-supported"), "two-sided-long-edge") ||
		slices.Contains(stringsFrom(printer, "sides-supported"), "two-sided-short-edge")
	caps.fields = map[string]string{
		"txtvers": "1", "qtotal": "1", "rp": strings.TrimPrefix(printerPath, "/"),
		"ty": clipTXT(caps.model), "pdl": strings.Join(advertised, ","),
		"Color": truth(color), "Duplex": truth(duplex),
	}
	if caps.airPrint {
		caps.fields["URF"] = strings.Join(urf, ",")
	}
	if uuid := first(stringsFrom(printer, "printer-uuid"), ""); strings.HasPrefix(uuid, "urn:uuid:") && len(uuid) == 45 {
		caps.fields["UUID"] = strings.TrimPrefix(uuid, "urn:uuid:")
	}
	return caps, nil
}

func hasValue(values goipp.Values, value goipp.Value) bool {
	for _, v := range values {
		if goipp.ValueEqual(v.V, value) {
			return true
		}
	}
	return false
}

// postIPP always connects to the configured physical IP and port, even for
// job-specific resource paths. No client headers, redirects or proxy settings
// can pick the destination.
func postIPP(ctx context.Context, client *http.Client, target *url.URL, body io.Reader, size int64) (*goipp.Message, error) {
	endpoint := *target
	endpoint.Scheme = "http"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/ipp")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("printer HTTP status %d", resp.StatusCode)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "application/ipp" {
		return nil, errors.New("printer did not return an IPP response")
	}
	var msg goipp.Message
	if err := msg.Decode(io.LimitReader(resp.Body, maxAttributes)); err != nil {
		return nil, fmt.Errorf("read printer IPP response: %w", err)
	}
	return &msg, nil
}

type ippProxy struct {
	client          *http.Client
	backend         *url.URL
	backendPrinter  string
	front           url.URL
	wifiAddress     string
	slots           chan struct{}
	legacy          *legacyFormats
	conversionSlots chan struct{}
	jobKey          [32]byte
}

func (p *ippProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != printerPath && !strings.HasPrefix(r.URL.Path, jobPathPrefix) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Use an IPP client.", http.StatusMethodNotAllowed)
		return
	}
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "application/ipp" || r.URL.RawQuery != "" ||
		(r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity") {
		http.Error(w, "Expected an IPP request.", http.StatusBadRequest)
		return
	}
	if r.ContentLength > maxJobBytes {
		http.Error(w, "Print job exceeds 256 MiB.", http.StatusRequestEntityTooLarge)
		return
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "Printer busy.", http.StatusServiceUnavailable)
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxJobBytes)
	defer body.Close()
	attrReader := &io.LimitedReader{R: body, N: maxAttributes}
	var msg goipp.Message
	if err := msg.Decode(attrReader); err != nil {
		http.Error(w, "Invalid IPP attributes.", http.StatusBadRequest)
		return
	}
	op := goipp.Op(msg.Code)
	if !slices.Contains(mobileOps, op) {
		ippError(w, &msg, goipp.StatusErrorOperationNotSupported, "Unsupported operation")
		return
	}
	target := *p.backend
	if r.URL.Path != printerPath && !slices.Contains(jobOps, op) {
		ippError(w, &msg, goipp.StatusErrorBadRequest, "Invalid job resource")
		return
	}

	if err := p.rewriteRequest(&msg, &target, r.URL.Path); err != nil {
		ippError(w, &msg, goipp.StatusErrorBadRequest, "Invalid printer or job URI")
		return
	}
	hasDocument := op == goipp.OpPrintJob || op == goipp.OpSendDocument
	document := io.Reader(body)
	documentSize := int64(-1)
	if r.ContentLength >= 0 {
		documentSize = r.ContentLength - (maxAttributes - attrReader.N)
	}
	if p.legacy != nil && (hasDocument || op == goipp.OpValidateJob || op == goipp.OpCreateJob) {
		format := first(stringsFrom(msg.Operation, "document-format"), "")
		if hasDocument {
			if first(stringsFrom(msg.Operation, "compression"), "none") != "none" {
				ippError(w, &msg, goipp.StatusErrorCompressionNotSupported, "Select uncompressed printing")
				return
			}
			if format == "" || format == "application/octet-stream" {
				reader := bufio.NewReader(document)
				start, _ := reader.Peek(8)
				format, document = detectedPrintFormat(start), reader
			}
		}
		if slices.Contains(p.legacy.convert, format) {
			if hasDocument {
				select {
				case p.conversionSlots <- struct{}{}:
					defer func() { <-p.conversionSlots }()
				default:
					ippError(w, &msg, goipp.StatusErrorBusy, "Print converter is busy")
					return
				}
				converted, size, cleanup, err := convertToPostScript(r.Context(), format, document)
				if err != nil {
					log.Printf("PostScript conversion failed: %v", err)
					ippError(w, &msg, goipp.StatusErrorDocumentFormatError, "Could not convert the document to PostScript")
					return
				}
				defer cleanup()
				document, documentSize = converted, size
			}
			for i := range msg.Groups {
				if msg.Groups[i].Tag == goipp.TagOperationGroup {
					setAttribute(&msg.Groups[i].Attrs, goipp.MakeAttr("document-format", goipp.TagMimeType, goipp.String(postScriptMIME)))
					break
				}
			}
		}
	}
	prefix, err := msg.EncodeBytes()
	if err != nil {
		ippError(w, &msg, goipp.StatusErrorBadRequest, "Invalid IPP attributes")
		return
	}
	data := io.Reader(bytes.NewReader(prefix))
	size := int64(len(prefix))
	if hasDocument {
		data = io.MultiReader(data, document)
		if documentSize < 0 {
			size = -1
		} else {
			size += documentSize
		}
	}

	resp, err := postIPP(r.Context(), p.client, &target, data, size)
	if err != nil {
		log.Printf("%s failed: %v", op, err)
		ippError(w, &msg, goipp.StatusErrorServiceUnavailable, "Ethernet printer unavailable")
		return
	}
	if resp.RequestID != msg.RequestID {
		ippError(w, &msg, goipp.StatusErrorServiceUnavailable, "Unexpected printer response")
		return
	}
	front := p.front
	if r.Host == p.wifiAddress {
		front.Host = p.wifiAddress
	}
	for i := range resp.Groups {
		resp.Groups[i].Attrs = p.rewriteResponse(resp.Groups[i].Attrs, front)
		if p.legacy != nil && op == goipp.OpGetPrinterAttributes && resp.Code < 0x0100 &&
			resp.Groups[i].Tag == goipp.TagPrinterGroup {
			p.legacy.advertise(&resp.Groups[i].Attrs, stringsFrom(msg.Operation, "requested-attributes"))
		}
	}
	writeIPP(w, resp)
}

func setAttribute(attrs *goipp.Attributes, attr goipp.Attribute) {
	for i := range *attrs {
		if (*attrs)[i].Name == attr.Name {
			(*attrs)[i] = attr
			return
		}
	}
	attrs.Add(attr)
}

func (legacy *legacyFormats) advertise(attrs *goipp.Attributes, requested []string) {
	all := len(requested) == 0 || slices.Contains(requested, "all") || slices.Contains(requested, "printer-description")
	for _, entry := range []struct {
		name   string
		tag    goipp.Tag
		values []string
	}{
		{"document-format-supported", goipp.TagMimeType, legacy.supported},
		{"urf-supported", goipp.TagKeyword, legacy.urf},
		{"compression-supported", goipp.TagKeyword, []string{"none"}},
	} {
		if !all && !slices.Contains(requested, entry.name) {
			continue
		}
		attr := goipp.Attribute{Name: entry.name}
		for _, value := range entry.values {
			attr.Values.Add(entry.tag, goipp.String(value))
		}
		setAttribute(attrs, attr)
	}
}

func (p *ippProxy) rewriteRequest(msg *goipp.Message, target *url.URL, requestPath string) error {
	found := false
	for _, group := range msg.Groups {
		if group.Tag != goipp.TagOperationGroup {
			continue
		}
		for i := range group.Attrs {
			a := &group.Attrs[i]
			if a.Name != "printer-uri" && a.Name != "job-uri" {
				continue
			}
			if len(a.Values) != 1 || a.Values[0].T != goipp.TagURI {
				return errors.New("invalid URI attribute")
			}
			value, ok := a.Values[0].V.(goipp.String)
			if !ok {
				return errors.New("invalid URI value")
			}
			if found {
				return errors.New("multiple target URIs")
			}
			found = true
			local, err := url.Parse(string(value))
			if err != nil || local.Scheme != "ipp" || local.Hostname() == "" || local.User != nil ||
				local.Fragment != "" || local.RawQuery != "" || local.ForceQuery {
				return errors.New("invalid local URI")
			}
			if a.Name == "printer-uri" && requestPath == printerPath && local.Path == printerPath {
				a.Values[0].V = goipp.String(p.backendPrinter)
				continue
			}
			if a.Name != "job-uri" || !slices.Contains(jobOps, goipp.Op(msg.Code)) ||
				(requestPath != printerPath && requestPath != local.Path) {
				return errors.New("invalid job target")
			}
			job, err := p.decodeJobPath(local.Path)
			if err != nil {
				return err
			}
			target.Path, target.RawPath, target.RawQuery = job.Path, job.RawPath, job.RawQuery
			upstream, _ := url.Parse(p.backendPrinter)
			upstream.Path, upstream.RawPath, upstream.RawQuery = job.Path, job.RawPath, job.RawQuery
			a.Values[0].V = goipp.String(upstream.String())
		}
	}
	if !found {
		return errors.New("missing target URI")
	}
	return nil
}

// Authenticate the original job resource so the Wi-Fi client cannot choose
// arbitrary HTTP paths on the printer. No document or job state is kept here.
func (p *ippProxy) jobPath(path string) string {
	mac := hmac.New(sha256.New, p.jobKey[:])
	_, _ = io.WriteString(mac, path)
	return jobPathPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum([]byte(path)))
}

func (p *ippProxy) decodeJobPath(path string) (*url.URL, error) {
	token, ok := strings.CutPrefix(path, jobPathPrefix)
	if !ok || len(token) > 2048 {
		return nil, errors.New("invalid job URI")
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(data) <= sha256.Size {
		return nil, errors.New("invalid job token")
	}
	rawPath, tag := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	mac := hmac.New(sha256.New, p.jobKey[:])
	_, _ = mac.Write(rawPath)
	if !hmac.Equal(mac.Sum(nil), tag) {
		return nil, errors.New("unrecognized printer job")
	}
	u, err := url.ParseRequestURI(string(rawPath))
	if err != nil || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") ||
		u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return nil, errors.New("invalid backend job path")
	}
	return u, nil
}

func (p *ippProxy) rewriteResponse(attrs goipp.Attributes, front url.URL) goipp.Attributes {
	result := attrs[:0]
	for _, attr := range attrs {
		// These reference web resources or transports the proxy doesn't serve.
		switch attr.Name {
		case "printer-more-info", "printer-more-info-manufacturer", "printer-icons", "printer-strings-uri",
			"printer-supply-info-uri", "printer-charge-info-uri", "printer-driver-installer",
			"printer-strings-languages-supported":
			continue
		case "uri-security-supported", "uri-authentication-supported", "xri-security", "xri-authentication":
			attr = goipp.MakeAttr(attr.Name, goipp.TagKeyword, goipp.String("none"))
		case "printer-uri-supported", "job-printer-uri", "xri-uri":
			attr = goipp.MakeAttr(attr.Name, goipp.TagURI, goipp.String(front.String()))
		case "job-uri":
			if value := first(stringsFrom(goipp.Attributes{attr}, attr.Name), ""); value != "" {
				u, err := url.Parse(value)
				if err == nil && (u.Scheme == "ipp" || u.Scheme == "ipps") && u.Path != "" {
					uFront := front
					uFront.Path = p.jobPath(u.RequestURI())
					attr = goipp.MakeAttr(attr.Name, goipp.TagURI, goipp.String(uFront.String()))
				}
			}
		case "operations-supported":
			original := attr.Values
			attr.Values = nil
			for _, v := range original {
				n, ok := v.V.(goipp.Integer)
				if ok && slices.Contains(mobileOps, goipp.Op(n)) {
					attr.Values = append(attr.Values, v)
				}
			}
			if len(attr.Values) == 0 {
				continue
			}
		default:
			for i, val := range attr.Values {
				if collection, ok := val.V.(goipp.Collection); ok {
					attr.Values[i].V = goipp.Collection(p.rewriteResponse(goipp.Attributes(collection), front))
				}
			}
		}
		result = append(result, attr)
	}
	return result
}

func ippError(w http.ResponseWriter, request *goipp.Message, status goipp.Status, info string) {
	resp := goipp.NewResponse(request.Version, status, request.RequestID)
	resp.Operation.Add(goipp.MakeAttr("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	resp.Operation.Add(goipp.MakeAttr("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	resp.Operation.Add(goipp.MakeAttr("status-message", goipp.TagText, goipp.String(info)))
	writeIPP(w, resp)
}

func writeIPP(w http.ResponseWriter, msg *goipp.Message) {
	data, err := msg.EncodeBytes()
	if err != nil {
		http.Error(w, "Invalid IPP response.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/ipp")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}
