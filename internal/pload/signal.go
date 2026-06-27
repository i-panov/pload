package pload

import (
	"os"
	"os/signal"
	"syscall"
)

func (d *Downloader) setupSignalHandler() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		d.logWarn("\nInterrupt signal received, saving state...")

		if d.config.SaveState {
			d.partsMu.Lock()
			if err := d.saveStateFileUnsafe(); err != nil {
				d.logError("Failed to save state on exit: %v", err)
			} else {
				d.logInfo("State saved to %s.parts", d.filePath)
			}
			d.partsMu.Unlock()
		}

		d.cancel()
	}()
}
