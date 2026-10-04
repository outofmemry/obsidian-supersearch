//go:build !darwin && !linux && !windows

package main

func batteryNow() bool { return false }
