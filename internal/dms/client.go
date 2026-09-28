// Package dms owns the external wallpaper IPC boundary.
package dms

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Client runs DMS IPC commands without shell interpolation.
type Client struct{ Path string }

// Current returns the path currently selected by DMS.
func (c Client) Current() (string, error) {
	output, err := exec.Command(c.Path, "ipc", "call", "wallpaper", "get").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// Set asks DMS to show the selected image path.
func (c Client) Set(path string) error {
	command := exec.Command(c.Path, "ipc", "call", "wallpaper", "set", path)
	var output bytes.Buffer
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("DMS no pudo aplicar fondo: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return nil
}
