//go:build darwin && !portable

package main

// macOS: Apple frameworks through the Swift helper (helper/main.swift).
// Build with -tags portable to use the Linux/Windows tools on a Mac instead.

import (
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	appleBackend = true
	mediaEngine  = "vision1"
)

func ocrImage(abs string, h *helperProc) (string, error) {
	if h != nil {
		// Through the worker's long-running helper: Vision's model stays
		// loaded, which halves the time and CPU per image vs a process each.
		var resp struct{ Answer, Error string }
		err := h.call(map[string]any{"op": "ocr", "path": abs, "langs": ocrLang}, &resp, 3*time.Minute)
		if err == nil && resp.Error != "" {
			err = errors.New(resp.Error)
		}
		return resp.Answer, err
	}
	return run(3*time.Minute, helperPath, "ocr", abs, ocrLang)
}

func transcribe(abs string) (string, error) {
	return runLow(30*time.Minute, helperPath, "transcribe", abs, ocrLang)
}

// pdfRaw returns the PDF's text layer, every page ended by \f.
func pdfRaw(abs string) (string, error) {
	return run(time.Minute, helperPath, "pdftext", abs)
}

// legacyText reads .doc .rtf .odt with macOS textutil.
func legacyText(abs string) (string, error) {
	return run(time.Minute, "textutil", "-convert", "txt", "-stdout", abs)
}

func askModel(ix *Index, instructions, prompt string) (string, error) {
	var resp struct{ Answer, Error string }
	err := ix.ah.call(map[string]any{"op": "ask", "instructions": instructions, "prompt": prompt}, &resp, 2*time.Minute)
	if err == nil && resp.Error != "" {
		err = fmt.Errorf("%s", resp.Error)
	}
	return resp.Answer, err
}

func audioMissing() string { return "" }

// missingTools names what can't run, with what it costs, for the plugin's notice.
func (ix *Index) missingTools() []string {
	if _, err := os.Stat(helperPath); err != nil {
		return []string{"supersearch-helper (PDFs, images, audio)"}
	}
	return []string{}
}
