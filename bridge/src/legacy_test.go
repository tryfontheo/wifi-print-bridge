package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectedPrintFormat(t *testing.T) {
	for _, tc := range []struct {
		prefix []byte
		want   string
	}{
		{[]byte("UNIRAST\x00"), "image/urf"},
		{[]byte("%PDF-1.7"), "application/pdf"},
		{[]byte("RaS2PwGR"), "image/pwg-raster"},
		{[]byte{0xff, 0xd8, 0xff, 0xe0}, "image/jpeg"},
		{[]byte("something"), ""},
		{[]byte("UNIRAST"), ""},
		{nil, ""},
	} {
		if got := detectedPrintFormat(tc.prefix); got != tc.want {
			t.Errorf("format of %q = %q; want %q", tc.prefix, got, tc.want)
		}
	}
}

func TestPostScriptSpoolCancelsBeforeExceedingLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	spool := postScriptSpool{file: &output, remaining: 3, cancel: cancel}
	if n, err := spool.Write([]byte("123")); n != 3 || err != nil {
		t.Fatalf("write to limit: %d, %v", n, err)
	}
	if n, err := spool.Write([]byte("4")); n != 0 || err == nil {
		t.Fatalf("write beyond limit: %d, %v", n, err)
	}
	if !spool.oversized || !errors.Is(ctx.Err(), context.Canceled) || output.String() != "123" {
		t.Fatal("oversized output was not stopped")
	}
}

func TestLegacyConverterProducesCompletePostScript(t *testing.T) {
	if !postScriptConverterAvailable() {
		t.Skip("Apple's PostScript print converter is unavailable")
	}
	t.Setenv("TMPDIR", t.TempDir())
	var doc bytes.Buffer
	if err := jpeg.Encode(&doc, image.NewGray(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	output, size, cleanup, err := convertToPostScript(t.Context(), "image/jpeg", &doc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	data, err := io.ReadAll(output)
	if err != nil || int64(len(data)) != size || !bytes.HasPrefix(data, []byte("%!PS")) || !bytes.Contains(data, []byte("showpage")) {
		t.Fatalf("invalid output: read %d bytes of %d, error %v", len(data), size, err)
	}
	dir := filepath.Dir(output.Name())
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("spool is not private: %v, %v", info, err)
	}
	cleanup()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spool remains after cleanup: %v", err)
	}
}

func TestLegacyConversionRemovesFailedJob(t *testing.T) {
	if !postScriptConverterAvailable() {
		t.Skip("Apple's PostScript print converter is unavailable")
	}
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	_, _, _, err := convertToPostScript(t.Context(), "application/pdf", bytes.NewBufferString("invalid PDF"))
	if err == nil {
		t.Fatal("invalid PDF accepted for conversion")
	}
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("spool remains after failed conversion: %v, %v", entries, err)
	}
}
