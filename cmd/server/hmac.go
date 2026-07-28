package main

import (
	"fmt"
	"os"
	"strings"
)

func loadHMACKey(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("--hmac-key-file is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read HMAC key file: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("HMAC key file is empty or whitespace-only: %s", path)
	}
	return key, nil
}
