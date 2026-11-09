package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func AppDataDir() (string, error) {
	if directory := os.Getenv("RAGFY_HOME"); directory != "" {
		return directory, nil
	}
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("LOCALAPPDATA not set")
		}
		return filepath.Join(base, "Ragfy"), nil
	}
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Ragfy"), err
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "ragfy"), nil
}

func FindExecutable(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err == nil {
		candidate := filepath.Join(home, ".local", "bin", name)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s executable not found: %w", name, err)
	}
	return path, nil
}

type ServiceState struct {
	Registered bool   `json:"registered"`
	Running    bool   `json:"running"`
	PID        string `json:"pid,omitempty"`
}

func InspectService() (ServiceState, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ServiceState{}, err
	}
	switch runtime.GOOS {
	case "darwin":
		path := filepath.Join(home, "Library", "LaunchAgents", "com.ragfy.service.plist")
		state := ServiceState{}
		if _, err := os.Stat(path); err == nil {
			state.Registered = true
		} else if !os.IsNotExist(err) {
			return state, err
		}
		output, commandErr := exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/com.ragfy.service", os.Getuid())).CombinedOutput()
		if commandErr != nil {
			return state, nil
		}
		for _, line := range strings.Split(string(output), "\n") {
			line = strings.TrimSpace(line)
			if line == "state = running" {
				state.Running = true
			}
			if strings.HasPrefix(line, "pid = ") {
				state.PID = strings.TrimSpace(strings.TrimPrefix(line, "pid = "))
			}
		}
		return state, nil
	case "linux":
		path := filepath.Join(home, ".config", "systemd", "user", "ragfy.service")
		state := ServiceState{}
		if _, err := os.Stat(path); err == nil {
			state.Registered = true
		} else if !os.IsNotExist(err) {
			return state, err
		}
		state.Running = exec.Command("systemctl", "--user", "is-active", "--quiet", "ragfy.service").Run() == nil
		if state.Running {
			if output, err := exec.Command("systemctl", "--user", "show", "--property", "MainPID", "--value", "ragfy.service").Output(); err == nil {
				state.PID = strings.TrimSpace(string(output))
			}
		}
		return state, nil
	case "windows":
		output, commandErr := exec.Command("schtasks", "/Query", "/TN", "RagfyService", "/FO", "LIST").CombinedOutput()
		if commandErr != nil {
			return ServiceState{}, nil
		}
		text := strings.ToLower(string(output))
		return ServiceState{Registered: true, Running: strings.Contains(text, "running")}, nil
	default:
		return ServiceState{}, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func LoadToken(path string) (string, error) {
	if contents, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(contents)), nil
	}
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(randomBytes)
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		return "", err
	}
	return token, nil
}

func WaitHealthy(ctx context.Context, listenAddress string, startedAt time.Time, output io.Writer) error {
	dataDirectory, err := AppDataDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dataDirectory, "logs", "service.log")
	lastProgress := ""
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		token, tokenErr := os.ReadFile(filepath.Join(dataDirectory, "token"))
		if tokenErr == nil {
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listenAddress+"/health", nil)
			request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
			response, requestErr := (&http.Client{Timeout: 2 * time.Second}).Do(request)
			if requestErr == nil {
				response.Body.Close()
				if response.StatusCode/100 == 2 {
					fmt.Fprintln(output, "✓ Ragfy service is healthy")
					return nil
				}
			}
		}
		if failure, found := StartupFailure(logPath, startedAt); found {
			return errors.New(failure)
		}
		if progress, found := StartupProgress(logPath); found && progress != lastProgress {
			fmt.Fprintln(output, "  "+progress)
			lastProgress = progress
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service did not become healthy: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func StartupFailure(path string, startedAt time.Time) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if strings.Contains(line, "startup failed:") {
			if len(line) >= 19 {
				stamp, stampErr := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local)
				if stampErr == nil && stamp.Before(startedAt.Add(-time.Second)) {
					return "", false
				}
			}
			return line[strings.Index(line, "startup failed:"):], true
		}
		if strings.Contains(line, "startup: ready") {
			return "", false
		}
	}
	return "", false
}

func StartupProgress(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false
	}
	const tailSize = 128 << 10
	start := info.Size() - tailSize
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(file, tailSize))
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		stamp, message := "", line
		if len(line) > 20 {
			if _, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], time.Local); err == nil {
				stamp, message = line[:19], strings.TrimSpace(line[19:])
			}
		}
		if strings.Contains(message, "startup: ready") {
			return "", false
		}
		isProgress := strings.Contains(message, "startup:") || strings.Contains(message, "download ") || strings.Contains(message, "baixando modelo ")
		if !isProgress {
			continue
		}
		message = strings.TrimSpace(strings.TrimPrefix(message, "startup:"))
		if stamp != "" {
			if timestamp, err := time.ParseInLocation("2006/01/02 15:04:05", stamp, time.Local); err == nil {
				age := time.Since(timestamp).Round(time.Second)
				if age < 0 {
					age = 0
				}
				return fmt.Sprintf("Service is not ready; last startup update %s ago: %s", age, message), true
			}
		}
		return "Service is not ready; last startup update: " + message, true
	}
	return "", false
}

func RequestIngest(path, project, listenAddress string, output io.Writer) error {
	dataDirectory, err := AppDataDir()
	if err != nil {
		return err
	}
	token, err := os.ReadFile(filepath.Join(dataDirectory, "token"))
	if err != nil {
		return errors.New("service is not installed; run ragfy setup")
	}
	body, _ := json.Marshal(map[string]string{"path": path, "project": project})
	request, err := http.NewRequest(http.MethodPost, "http://"+listenAddress+"/ingest", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 30 * time.Minute}).Do(request)
	if err != nil {
		return errors.New("service is not running; run `ragfy setup`")
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("ingest failed: %s", strings.TrimSpace(string(responseBody)))
	}
	fmt.Fprintln(output, string(responseBody))
	return nil
}

func InstallService(executable string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		directory := filepath.Join(home, "Library", "LaunchAgents")
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		path := filepath.Join(directory, "com.ragfy.service.plist")
		contents := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>com.ragfy.service</string><key>ProgramArguments</key><array><string>%s</string><string>serve</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/></dict></plist>`, executable)
		_ = exec.Command("launchctl", "unload", path).Run()
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			return err
		}
		return exec.Command("launchctl", "load", path).Run()
	case "linux":
		directory := filepath.Join(home, ".config", "systemd", "user")
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		contents := fmt.Sprintf("[Unit]\nDescription=Ragfy local memory service\nAfter=default.target\n\n[Service]\nExecStart=%s serve\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n", executable)
		if err := os.WriteFile(filepath.Join(directory, "ragfy.service"), []byte(contents), 0600); err != nil {
			return err
		}
		if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
			return err
		}
		if err := exec.Command("systemctl", "--user", "enable", "ragfy.service").Run(); err != nil {
			return err
		}
		return exec.Command("systemctl", "--user", "restart", "ragfy.service").Run()
	case "windows":
		arguments := fmt.Sprintf(`"%s" serve`, executable)
		_ = exec.Command("schtasks", "/End", "/TN", "RagfyService").Run()
		if err := exec.Command("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", "RagfyService", "/TR", arguments).Run(); err != nil {
			return err
		}
		return exec.Command("schtasks", "/Run", "/TN", "RagfyService").Run()
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func UninstallService() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		path := filepath.Join(home, "Library", "LaunchAgents", "com.ragfy.service.plist")
		_ = exec.Command("launchctl", "unload", path).Run()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "linux":
		_ = exec.Command("systemctl", "--user", "disable", "--now", "ragfy.service").Run()
		_ = os.Remove(filepath.Join(home, ".config", "systemd", "user", "ragfy.service"))
		return exec.Command("systemctl", "--user", "daemon-reload").Run()
	case "windows":
		return exec.Command("schtasks", "/Delete", "/F", "/TN", "RagfyService").Run()
	}
	return nil
}
