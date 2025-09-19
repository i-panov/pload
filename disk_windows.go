//go:build windows

package main

import (
	"fmt"
	"golang.org/x/sys/windows"
)

// checkDiskSpacePlatform — реализация для Windows.
func (d *Downloader) checkDiskSpacePlatform(dir string, required int64) error {
	var freeBytesAvailable uint64
	
	// Преобразуем путь в UTF16 для WinAPI
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return fmt.Errorf("windows: invalid path: %w", err)
	}

	// Вызываем функцию WinAPI GetDiskFreeSpaceEx
	err = windows.GetDiskFreeSpaceEx(dirPtr, &freeBytesAvailable, nil, nil)
	if err != nil {
		return fmt.Errorf("windows: GetDiskFreeSpaceEx failed: %w", err)
	}

	if int64(freeBytesAvailable) < required {
		return fmt.Errorf("недостаточно места: нужно %s, доступно %s", formatBytes(required), formatBytes(int64(freeBytesAvailable)))
	}

	d.logDebug("Диск: нужно %s, доступно %s", formatBytes(required), formatBytes(int64(freeBytesAvailable)))
	return nil
}
