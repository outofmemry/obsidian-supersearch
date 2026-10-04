package main

import (
	"syscall"
	"unsafe"
)

var getSystemPowerStatus = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemPowerStatus")

// batteryNow asks Windows whether the AC line is offline (SYSTEM_POWER_STATUS).
func batteryNow() bool {
	var s struct {
		ACLineStatus, BatteryFlag, BatteryLifePercent, SystemStatusFlag byte
		BatteryLifeTime, BatteryFullLifeTime                            uint32
	}
	if r, _, _ := getSystemPowerStatus.Call(uintptr(unsafe.Pointer(&s))); r == 0 {
		return false
	}
	return s.ACLineStatus == 0 // 0 offline, 1 online, 255 unknown
}
