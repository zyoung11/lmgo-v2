package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

//go:embed favicon.ico
var iconData []byte

//go:embed default_config.json
var defaultConfigData []byte

//go:embed *.zip
var serverArchives embed.FS

type Config struct {
	AutoStartEnabled bool     `json:"autoStartEnabled"`
	Port             int      `json:"port"`
	PollInterval     int      `json:"pollInterval"`
}

type modelSection struct {
	Name string
	Path string
}

var (
	config        Config
	serverCmd     *exec.Cmd
	serverCmdMu   sync.Mutex
	modelSections []modelSection
	currentModel  string
	currentModelMu sync.RWMutex

	menuItems struct {
		modelLabel   *systray.MenuItem
		webInterface *systray.MenuItem
		autoStart    *systray.MenuItem
		quit         *systray.MenuItem
	}
)

func main() {
	hideConsole()

	if exePath, err := os.Executable(); err == nil {
		if err := os.Chdir(filepath.Dir(exePath)); err != nil {
			log.Printf("Warning: Failed to change working directory: %v", err)
		}
	}

	if err := loadConfig(); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	regEnabled, _ := isAutoStartEnabled()
	if regEnabled != config.AutoStartEnabled {
		config.AutoStartEnabled = regEnabled
		saveConfig()
	}

	if err := extractServer(); err != nil {
		log.Fatalf("Failed to extract server: %v", err)
	}

	if err := generateModelsINI(); err != nil {
		log.Fatalf("Failed to generate models.ini: %v", err)
	}

	if err := startLlamaServer(); err != nil {
		log.Fatalf("Failed to start llama-server: %v", err)
	}

	systray.Run(onReady, onExit)
}

func hideConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleWindow := kernel32.NewProc("GetConsoleWindow")
	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		return
	}
	user32 := syscall.NewLazyDLL("user32.dll")
	showWindow := user32.NewProc("ShowWindow")
	showWindow.Call(hwnd, 0)
}

func loadConfig() error {
	configFile := "config.json"
	data, err := os.ReadFile(configFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read config: %v", err)
		}
		if err := json.Unmarshal(defaultConfigData, &config); err != nil {
			return fmt.Errorf("failed to parse embedded default config: %v", err)
		}
		return saveConfig()
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}
	if config.Port == 0 {
		config.Port = 19966
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 2
	}
	return nil
}

func defaultArgs() []string {
	return []string{
		"--host", "0.0.0.0",
		"--no-host",
		"--prio-batch", "3",
		"--ctx-size", "131072",
		"--batch-size", "4096",
		"--ubatch-size", "4096",
		"--threads", "0",
		"--threads-batch", "0",
		"-ngl", "999",
		"--flash-attn", "on",
		"--cache-type-k", "f16",
		"--cache-type-v", "f16",
		"--kv-offload",
		"--no-mmap",
		"--no-repack",
		"--direct-io",
		"--mlock",
		"--split-mode", "layer",
		"--main-gpu", "0",
	}
}

func saveConfig() error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode config: %v", err)
	}
	return os.WriteFile("config.json", data, 0644)
}

func extractServer() error {
	serverDir := "server"
	serverExe := filepath.Join(serverDir, "llama-server.exe")
	if _, err := os.Stat(serverExe); err == nil {
		return nil
	}
	if err := os.MkdirAll(serverDir, 0755); err != nil {
		return err
	}
	entries, err := serverArchives.ReadDir(".")
	if err != nil {
		return fmt.Errorf("failed to read embedded archives: %v", err)
	}
	if len(entries) != 1 {
		return fmt.Errorf("expected exactly one embedded zip, found %d", len(entries))
	}
	zipData, err := serverArchives.ReadFile(entries[0].Name())
	if err != nil {
		return fmt.Errorf("failed to read embedded zip: %v", err)
	}
	zipReader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return err
	}
	for _, file := range zipReader.File {
		target := filepath.Join(serverDir, file.Name)
		if file.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0755)
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, file.Mode())
		if err != nil {
			return err
		}
		src, err := file.Open()
		if err != nil {
			dst.Close()
			return err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func generateModelsINI() error {
	iniPath := "models.ini"
	if existing, err := os.ReadFile(iniPath); err == nil {
		modelSections = parseINISections(string(existing))
		return nil
	}

	var sb strings.Builder
	sb.WriteString("# lmgo models.ini\n")
	sb.WriteString("# Edit this file to define your models.\n")
	sb.WriteString("# Section name = model identifier used in API requests.\n\n")

	args := defaultArgs()
	if len(args) > 0 {
		sb.WriteString("[*]\n")
		for i := 0; i < len(args); i += 2 {
			if i+1 < len(args) {
				key := strings.TrimPrefix(args[i], "--")
				key = strings.TrimPrefix(key, "-")
				fmt.Fprintf(&sb, "%s = %s\n", key, args[i+1])
			} else {
				key := strings.TrimPrefix(args[i], "--")
				key = strings.TrimPrefix(key, "-")
				fmt.Fprintf(&sb, "%s = true\n", key)
			}
		}
		sb.WriteString("\n")
	}

	return os.WriteFile(iniPath, []byte(sb.String()), 0644)
}

func parseINISections(content string) []modelSection {
	var sections []modelSection
	var currentName string
	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			currentName = trimmed[1 : len(trimmed)-1]
			if currentName == "*" {
				currentName = ""
			}
			continue
		}
		if currentName != "" && strings.HasPrefix(trimmed, "model ") {
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) == 2 {
				sections = append(sections, modelSection{
					Name: currentName,
					Path: strings.TrimSpace(parts[1]),
				})
			}
		}
	}
	return sections
}

func stopLlamaServer() {
	if serverCmd == nil || serverCmd.Process == nil {
		return
	}
	exec.Command("taskkill", "/f", "/t", "/pid", strconv.Itoa(serverCmd.Process.Pid)).Run()

	done := make(chan struct{})
	go func() {
		serverCmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	serverCmd = nil
	time.Sleep(300 * time.Millisecond)
}

func startLlamaServer() error {
	serverCmdMu.Lock()
	stopLlamaServer()
	defer serverCmdMu.Unlock()

	args := []string{
		"--models-preset", "models.ini",
		"--port", strconv.Itoa(config.Port),
		"--host", "0.0.0.0",
	}

	serverExe := filepath.Join("server", "llama-server.exe")
	cmd := exec.Command(serverExe, args...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	cmd.Stdout = os.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start llama-server: %v", err)
	}

	serverCmd = cmd

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("llama-server exited: %v", err)
		}
		serverCmdMu.Lock()
		serverCmd = nil
		serverCmdMu.Unlock()
	}()

	return waitForServer(config.Port, 60*time.Second)
}

func waitForServer(port int, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)

	for {
		select {
		case <-ticker.C:
			resp, err := client.Get(url)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		case <-deadline:
			return fmt.Errorf("timeout waiting for llama-server to start on port %d", port)
		}
	}
}

func trackCurrentModel() {
	ticker := time.NewTicker(time.Duration(config.PollInterval) * time.Second)
	defer ticker.Stop()

	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/models", config.Port)

	for range ticker.C {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}

		var result struct {
			Data []struct {
				ID     string `json:"id"`
				Status struct {
					Value string `json:"value"`
				} `json:"status"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		var loaded string
		for _, m := range result.Data {
			if m.Status.Value == "loaded" {
				loaded = m.ID
				break
			}
		}

		currentModelMu.Lock()
		changed := currentModel != loaded
		currentModel = loaded
		currentModelMu.Unlock()

		if changed {
			refreshMenuState()
		}
	}
}

func onReady() {
	systray.SetIcon(iconData)
	systray.SetTitle("lmgo")
	systray.SetTooltip("lmgo Router Server")

	buildMenu()
	refreshMenuState()

	go trackCurrentModel()

	log.Printf("lmgo v2 started. http://localhost:%d, Models: %d",
		config.Port, len(modelSections))

}

func buildMenu() {
	menuItems.modelLabel = systray.AddMenuItem("Idle", "Currently loaded model")
	menuItems.modelLabel.Disable()
	systray.AddSeparator()
	menuItems.webInterface = systray.AddMenuItem("Web Interface", "Open web UI")
	menuItems.autoStart = systray.AddMenuItem("Auto Startup", "Toggle auto-start")
	systray.AddSeparator()
	menuItems.quit = systray.AddMenuItem("Exit", "Quit lmgo")

	go func() {
		for range menuItems.webInterface.ClickedCh {
			url := fmt.Sprintf("http://127.0.0.1:%d", config.Port)
			exec.Command("cmd", "/c", "start", url).Start()
		}
	}()

	go func() {
		for range menuItems.autoStart.ClickedCh {
			config.AutoStartEnabled = !config.AutoStartEnabled
			setAutoStart(config.AutoStartEnabled)
			saveConfig()
			refreshMenuState()
		}
	}()

	go func() {
		for range menuItems.quit.ClickedCh {
			systray.Quit()
		}
	}()
}

func refreshMenuState() {
	currentModelMu.RLock()
	model := currentModel
	currentModelMu.RUnlock()

	if model == "" {
		menuItems.modelLabel.SetTitle("Idle")
		menuItems.modelLabel.Disable()
	} else {
		menuItems.modelLabel.SetTitle("● " + model)
		menuItems.modelLabel.Enable()
	}

	if config.AutoStartEnabled {
		menuItems.autoStart.SetTitle("✓ Auto Startup")
	} else {
		menuItems.autoStart.SetTitle("Auto Startup")
	}
}

func startupShortcutPath() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", fmt.Errorf("APPDATA not set")
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "lmgo-v2.lnk"), nil
}

func setAutoStart(enabled bool) error {
	shortcutPath, err := startupShortcutPath()
	if err != nil {
		return err
	}
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %v", err)
	}
	if enabled {
		psCmd := fmt.Sprintf(
			"$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%s'); $s.TargetPath = '%s'; $s.Arguments = '--autostart'; $s.WorkingDirectory = '%s'; $s.WindowStyle = 7; $s.Save()",
			shortcutPath, exePath, filepath.Dir(exePath),
		)
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psCmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to create startup shortcut: %v (output: %s)", err, string(output))
		}
		log.Printf("Startup shortcut created: %s", shortcutPath)
	} else {
		os.Remove(shortcutPath)
		log.Printf("Startup shortcut removed")
	}
	return nil
}

func isAutoStartEnabled() (bool, error) {
	shortcutPath, err := startupShortcutPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(shortcutPath)
	return err == nil, nil
}

func onExit() {
	serverCmdMu.Lock()
	stopLlamaServer()
	serverCmdMu.Unlock()
}
