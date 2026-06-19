package auth

import (
	"os/exec"
	"runtime"
)

// openBrowser opens rawURL in the user's default system browser.
func openBrowser(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", rawURL)
	default: // linux and others
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}
