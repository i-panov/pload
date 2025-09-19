//go:build !unix && !windows

package main

import "errors"

// checkDiskSpacePlatform — заглушка для неподдерживаемых систем.
func (d *Downloader) checkDiskSpacePlatform(dir string, required int64) error {
	return errors.New("проверка дискового пространства не реализована для этой ОС")
}
