//go:build unix

package main

import (
	"fmt"
	"syscall"
)

// checkDiskSpacePlatform — реализация для Unix-подобных систем.
func (d *Downloader) checkDiskSpacePlatform(dir string, required int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("syscall.Statfs failed: %w", err)
	}

	// Bsize — это фундаментальный размер блока файловой системы.
	// Bavail — это количество свободных блоков, доступных непривилегированным пользователям.
	avail := int64(st.Bavail) * int64(st.Bsize)
	if avail < required {
		return fmt.Errorf("недостаточно места: нужно %s, доступно %s", formatBytes(required), formatBytes(avail))
	}
	d.logDebug("Диск: нужно %s, доступно %s", formatBytes(required), formatBytes(avail))
	return nil
}
