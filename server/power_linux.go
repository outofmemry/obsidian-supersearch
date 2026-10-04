package main

import (
	"os"
	"path/filepath"
	"strings"
)

// batteryNow: a battery that reports Discharging means no charger is plugged
// in. Desktops have no battery at all, so they always count as mains power.
func batteryNow() bool {
	dirs, _ := filepath.Glob("/sys/class/power_supply/*")
	for _, d := range dirs {
		typ, _ := os.ReadFile(filepath.Join(d, "type"))
		if strings.TrimSpace(string(typ)) != "Battery" {
			continue
		}
		if st, _ := os.ReadFile(filepath.Join(d, "status")); strings.TrimSpace(string(st)) == "Discharging" {
			return true
		}
	}
	return false
}
