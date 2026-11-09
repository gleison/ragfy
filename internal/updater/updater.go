package updater

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const InstallerURL = "https://raw.githubusercontent.com/gleison/ragfy/main/install.sh"

func Run(input io.Reader, output, errorOutput io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	prefix, err := installationPrefix(executable)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Get(InstallerURL)
	if err != nil {
		return fmt.Errorf("download installer: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("download installer: %s", response.Status)
	}
	temporary, err := os.CreateTemp("", "ragfy-install-*.sh")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer os.Remove(path)
	if err := temporary.Chmod(0700); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, io.LimitReader(response.Body, 1<<20)); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}

	command := exec.Command("/bin/sh", path, "--no-setup", "--prefix", prefix)
	command.Stdin, command.Stdout, command.Stderr = input, output, errorOutput
	if err := command.Run(); err != nil {
		return fmt.Errorf("install update: %w", err)
	}
	return nil
}

func installationPrefix(executable string) (string, error) {
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	if strings.Contains(executable, "/Cellar/") || strings.Contains(executable, "/homebrew/") {
		return "", fmt.Errorf("this installation is managed by Homebrew; run `brew upgrade ragfy`")
	}
	binDirectory := filepath.Dir(executable)
	if filepath.Base(binDirectory) != "bin" {
		return "", fmt.Errorf("cannot update an executable outside a bin directory: %s", executable)
	}
	return filepath.Dir(binDirectory), nil
}
