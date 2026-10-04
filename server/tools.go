package main

// Finding external tools: on PATH, or in their usual install folders, since
// Obsidian started from a desktop launcher often has a shorter PATH than a
// terminal, and on Windows installers rarely touch PATH at all.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var toolCache struct {
	sync.Mutex
	m map[string]toolHit
}

type toolHit struct {
	path string
	at   time.Time
}

// findTool returns the first of names found, or "". A miss is re-checked
// after a minute, so installing a tool needs no restart.
func findTool(names ...string) string {
	key := strings.Join(names, "|")
	toolCache.Lock()
	defer toolCache.Unlock()
	if hit, ok := toolCache.m[key]; ok && (hit.path != "" || time.Since(hit.at) < time.Minute) {
		return hit.path
	}
	if toolCache.m == nil {
		toolCache.m = map[string]toolHit{}
	}
	p := lookTool(names)
	toolCache.m[key] = toolHit{p, time.Now()}
	return p
}

func lookTool(names []string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	for _, dir := range toolDirs() {
		for _, n := range names {
			matches, _ := filepath.Glob(filepath.Join(dir, n+exe))
			for _, m := range matches {
				if st, err := os.Stat(m); err == nil && !st.IsDir() {
					return m
				}
			}
		}
	}
	return ""
}

func toolDirs() []string {
	home, _ := os.UserHomeDir()
	// Tools the installer unpacked itself (whisper.cpp on Windows).
	dirs := []string{filepath.Join(configDir(), "tools", "*")}
	if self, err := os.Executable(); err == nil {
		if self, err = filepath.EvalSymlinks(self); err == nil {
			dirs = append(dirs, filepath.Dir(self), filepath.Join(filepath.Dir(self), "tools", "*"))
		}
	}
	if runtime.GOOS == "windows" {
		pf, pf86, local, data := os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA"), os.Getenv("ProgramData")
		winget := filepath.Join(local, "Microsoft", "WinGet")
		return append(dirs,
			filepath.Join(pf, "Tesseract-OCR"), filepath.Join(pf86, "Tesseract-OCR"),
			filepath.Join(local, "Programs", "Tesseract-OCR"),
			filepath.Join(winget, "Links"),
			filepath.Join(winget, "Packages", "*", "*", "Library", "bin"), // poppler
			filepath.Join(winget, "Packages", "*", "*", "bin"),            // ffmpeg, whisper
			filepath.Join(winget, "Packages", "*", "*"),
			filepath.Join(home, "scoop", "shims"),
			filepath.Join(data, "chocolatey", "bin"),
		)
	}
	return append(dirs, filepath.Join(home, ".local", "bin"), "/usr/local/bin", "/opt/homebrew/bin",
		"/home/linuxbrew/.linuxbrew/bin", "/snap/bin")
}
