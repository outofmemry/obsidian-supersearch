package main

import (
	"bytes"
	"os/exec"
)

func batteryNow() bool {
	out, _ := exec.Command("pmset", "-g", "batt").Output()
	return bytes.Contains(out, []byte("Battery Power"))
}
