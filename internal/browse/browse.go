// Package browse opens a URL in the user's default browser.
package browse

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Open launches rawURL in the default browser without waiting for it to exit.
// $BROWSER, if set, takes precedence on every platform.
func Open(rawURL string) error {
	name, args := command(rawURL)
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opening %s with %s: %w", rawURL, name, err)
	}
	// Reap the child in the background so it does not linger as a zombie.
	go cmd.Wait()
	return nil
}

func command(rawURL string) (string, []string) {
	if b := strings.TrimSpace(os.Getenv("BROWSER")); b != "" {
		fields := strings.Fields(b)
		return fields[0], append(fields[1:], rawURL)
	}
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{rawURL}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		return "xdg-open", []string{rawURL}
	}
}
