package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const postScriptConverter = "/usr/libexec/cups/command/ippeveps"
const pdfPostScriptConverter = "/usr/libexec/cups/filter/cgpdftops"
const postScriptRenderTimeout = 2 * time.Minute

func postScriptConverterAvailable() bool {
	for _, path := range []string{postScriptConverter, pdfPostScriptConverter} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return false
		}
	}
	return true
}

func detectedPrintFormat(prefix []byte) string {
	switch {
	case bytes.HasPrefix(prefix, []byte("UNIRAST\x00")):
		return "image/urf"
	case bytes.HasPrefix(prefix, []byte("%PDF-")):
		return "application/pdf"
	case bytes.HasPrefix(prefix, []byte("RaS2")):
		return "image/pwg-raster"
	case bytes.HasPrefix(prefix, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	default:
		return ""
	}
}

// Render to a complete, bounded spool before the printer can receive any bytes.
// The caller owns the resulting file until it calls cleanup.
func convertToPostScript(ctx context.Context, format string, body io.Reader) (
	converted *os.File, size int64, cleanup func(), err error,
) {
	switch format {
	case "image/urf", "application/pdf", "image/pwg-raster", "image/jpeg":
	default:
		return nil, 0, nil, errors.New("document format cannot be converted to PostScript")
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, nil, err
	}

	dir, err := os.MkdirTemp("", "print-bridge-render-")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("create print spool: %w", err)
	}
	var output *os.File
	removeSpool := func() {
		if output != nil {
			_ = output.Close()
		}
		_ = os.RemoveAll(dir)
	}
	defer func() {
		if err != nil {
			removeSpool()
		}
	}()

	input, err := os.CreateTemp(dir, "input-")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("create input spool: %w", err)
	}
	spoolErr := readRenderInput(input, body)
	closeErr := input.Close()
	if spoolErr != nil {
		return nil, 0, nil, spoolErr
	}
	if closeErr != nil {
		return nil, 0, nil, fmt.Errorf("close input spool: %w", closeErr)
	}
	output, err = os.CreateTemp(dir, "postscript-")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("create output spool: %w", err)
	}

	renderCtx, cancel := context.WithTimeout(ctx, postScriptRenderTimeout)
	defer cancel()
	cmd := postScriptRenderCommand(renderCtx, format, input.Name())
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=/usr/bin:/bin:/usr/sbin",
		"TMPDIR=" + dir,
		"CONTENT_TYPE=" + format,
		"OUTPUT_FORMAT=application/postscript",
		"FINAL_CONTENT_TYPE=application/postscript",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	writer := &postScriptSpool{
		file: output, remaining: maxJobBytes, cancel: cancel,
	}
	cmd.Stdout = writer
	cmd.Stderr = io.Discard
	renderErr := cmd.Run()
	if writer.oversized {
		return nil, 0, nil, errors.New("converted print job exceeds 256 MiB")
	}
	if err := renderCtx.Err(); err != nil {
		return nil, 0, nil, fmt.Errorf("convert document to PostScript: %w", err)
	}
	if renderErr != nil {
		return nil, 0, nil, fmt.Errorf("convert document to PostScript: %w", renderErr)
	}

	var header [4]byte
	if _, err := output.ReadAt(header[:], 0); err != nil || string(header[:]) != "%!PS" {
		return nil, 0, nil, errors.New("renderer did not produce a PostScript document")
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return nil, 0, nil, fmt.Errorf("rewind converted print job: %w", err)
	}
	return output, maxJobBytes - writer.remaining, removeSpool, nil
}

func postScriptRenderCommand(ctx context.Context, format, input string) *exec.Cmd {
	if format == "application/pdf" {
		// ippeveps uses this filter, but stages its entire output before sending
		// any bytes to stdout. Invoking it directly bounds that output instead.
		return exec.CommandContext(ctx, pdfPostScriptConverter, "1", "print-bridge", "Print job", "1", "", input)
	}
	return exec.CommandContext(ctx, postScriptConverter, input)
}

func readRenderInput(output io.Writer, body io.Reader) error {
	n, err := io.Copy(output, io.LimitReader(body, maxJobBytes))
	if err != nil {
		return fmt.Errorf("read print job: %w", err)
	}
	if n == 0 {
		return errors.New("print job is empty")
	}
	if n == maxJobBytes {
		var extra [1]byte
		n, err := io.ReadFull(body, extra[:])
		if n != 0 {
			return errors.New("print job exceeds 256 MiB")
		}
		if err != io.EOF {
			return fmt.Errorf("read print job: %w", err)
		}
	}
	return nil
}

type postScriptSpool struct {
	file      io.Writer
	remaining int64
	cancel    context.CancelFunc
	oversized bool
}

func (s *postScriptSpool) Write(p []byte) (int, error) {
	if int64(len(p)) > s.remaining {
		s.oversized = true
		s.cancel()
		return 0, errors.New("PostScript output exceeds the print job size limit")
	}
	n, err := s.file.Write(p)
	s.remaining -= int64(n)
	return n, err
}
