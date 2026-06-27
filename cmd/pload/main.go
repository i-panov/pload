package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/i-panov/pload/internal/pload"
)

func main() {
	cfg, err := pload.ParseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	dl, err := pload.NewDownloader(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating downloader: %v\n", err)
		os.Exit(1)
	}

	if err := dl.Download(); err != nil {
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "operation cancelled by user") {
			dl.LogError("Download error: %v", err)
		} else {
			dl.LogInfo("Download cancelled.")
		}
		os.Exit(1)
	}
}
