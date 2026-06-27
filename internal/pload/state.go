package pload

import (
	"encoding/json"
	"fmt"
	"os"
)

type PartInfo struct {
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	Downloaded int64 `json:"downloaded"`
	Completed  bool  `json:"completed"`
}

func (d *Downloader) loadStateFile() error {
	if !d.config.SaveState {
		return nil
	}
	stateFile := d.filePath + ".parts"
	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading state file: %w", err)
	}

	d.partsMu.Lock()
	defer d.partsMu.Unlock()
	return json.Unmarshal(data, &d.parts)
}

func (d *Downloader) saveStateFileUnsafe() error {
	if !d.config.SaveState || len(d.parts) == 0 {
		return nil
	}
	stateFile := d.filePath + ".parts"
	data, err := json.MarshalIndent(d.parts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0644)
}

func (d *Downloader) removeStateFile() {
	if !d.config.SaveState {
		return
	}
	stateFile := d.filePath + ".parts"
	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		d.logWarn("Failed to remove state file %s: %v", stateFile, err)
	} else {
		d.logDebug("State file removed: %s", stateFile)
	}
}

func (d *Downloader) initializeParts(contentLength int64, numWorkers int) {
	d.partsMu.Lock()
	defer d.partsMu.Unlock()

	if len(d.parts) == numWorkers {
		return
	}

	d.parts = make([]PartInfo, numWorkers)
	partSize := contentLength / int64(numWorkers)

	for i := 0; i < numWorkers; i++ {
		start := int64(i) * partSize
		end := start + partSize - 1
		if i == numWorkers-1 {
			end = contentLength - 1
		}
		d.parts[i] = PartInfo{Start: start, End: end}
	}
}

func (d *Downloader) calculateInitialDownloaded() int64 {
	d.partsMu.Lock()
	defer d.partsMu.Unlock()
	var total int64
	for _, p := range d.parts {
		total += p.Downloaded
	}
	return total
}
