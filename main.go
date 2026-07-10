package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
)

//go:embed favicon.ico
var iconData []byte

//go:embed default_config.json
var defaultConfigData []byte

//go:embed *.zip
var serverArchives embed.FS

type Config struct {
	AutoStartEnabled bool `json:"autoStartEnabled"`
	Port             int  `json:"port"`
	PollInterval     int  `json:"pollInterval"`
	ModelsMax        int  `json:"modelsMax"`
	LogViewerPort    int  `json:"logViewerPort"`
}

type modelSection struct {
	Name string
	Path string
}

// LogCapture is a bounded, thread-safe byte buffer. When full, the
// oldest bytes are dropped so memory stays at cap.
type LogCapture struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

// serverLog collects llama-server stdio and lmgo runtime output for the
// View Log menu and Refresh-failure dialogs.
var serverLog = &LogCapture{cap: 256 * 1024}

func (l *LogCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > l.cap {
		l.buf = l.buf[len(l.buf)-l.cap:]
	}
	return len(p), nil
}

func (l *LogCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.buf)
}

func (l *LogCapture) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = l.buf[:0]
}

// priorityWriter routes writes through primary, falling back to secondary
// on error. Replaces io.MultiWriter, which short-circuits on the first
// failing writer and would drop bytes to os.Stdout under the GUI
// subsystem's NULL handle.
type priorityWriter struct {
	primary, secondary io.Writer
}

func (w *priorityWriter) Write(p []byte) (int, error) {
	n, err := w.primary.Write(p)
	if err == nil {
		return n, nil
	}
	return w.secondary.Write(p)
}

// llmProcess tracks the running llama-server child so Process.Wait is
// invoked exactly once per child and stop signals synchronize via the
// done channel rather than re-calling Wait (which would panic).
type llmProcess struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

var proc llmProcess

var (
	config         Config
	modelSections  []modelSection
	currentModel   string
	currentModelMu sync.RWMutex

	menuItems struct {
		modelLabel   *systray.MenuItem
		webInterface *systray.MenuItem
		refresh      *systray.MenuItem
		viewLog      *systray.MenuItem
		autoStart    *systray.MenuItem
		quit         *systray.MenuItem
	}
)

func main() {
	hideConsole()
	log.SetOutput(&priorityWriter{primary: os.Stderr, secondary: serverLog})

	if exePath, err := os.Executable(); err == nil {
		if err := os.Chdir(filepath.Dir(exePath)); err != nil {
			log.Printf("Warning: Failed to change working directory: %v", err)
		}
	}

	if err := loadConfig(); err != nil {
		fatalExit("Config error", "Failed to load config: %v", err)
	}

	regEnabled, _ := isAutoStartEnabled()
	if regEnabled != config.AutoStartEnabled {
		config.AutoStartEnabled = regEnabled
		saveConfig()
	}

	if err := extractServer(); err != nil {
		fatalExit("Server error", "Failed to extract server: %v", err)
	}

	if err := generateModelsINI(); err != nil {
		fatalExit("Config error", "Failed to generate models.ini: %v", err)
	}

	if err := startLlamaServer(); err != nil {
		msg := fmt.Sprintf("Failed to start llama-server: %v\n\n--- llama-server output ---\n%s", err, serverLog.String())
		fatalExit("Startup error", "%s", msg)
	}

	logViewerURL = fmt.Sprintf("http://127.0.0.1:%d/log/view", config.LogViewerPort)
	if err := initLogViewerServer(); err != nil {
		log.Printf("Warning: log viewer server failed to start: %v (View Log will be static only)", err)
		logViewerURL = ""
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

// fatalExit shows an error dialog and exits. Used at startup so fatal
// failures reach the user despite the hidden GUI console.
func fatalExit(title, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	u32 := syscall.NewLazyDLL("user32.dll")
	titlePtr, _ := syscall.UTF16PtrFromString("lmgo-v2: " + title)
	textPtr, _ := syscall.UTF16PtrFromString(msg)
	u32.NewProc("MessageBoxW").Call(
		0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		0x10,
	)
	os.Exit(1)
}

// logViewerHTML is the self-contained live-log page served at /log/view.
// The inline JS polls /log every 2s and appends only the new tail bytes,
// preserving scroll position. Inline styling keeps external requests to
// zero so SmartScreen and CSP concerns don't apply.
const logViewerHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>lmgo-v2 log</title>
<style>
  html, body { margin: 0; padding: 0; }
  body { background: #1e1e1e; color: #dddddd; font-family: Consolas, "Courier New", monospace; font-size: 13px; padding: 8px 12px; box-sizing: border-box; }
  pre#log { white-space: pre-wrap; word-break: break-all; margin: 0; }
  #status { position: fixed; top: 6px; right: 12px; opacity: 0.55; font-size: 11px; user-select: none; }
</style>
</head>
<body>
<pre id="log"></pre>
<div id="status">connecting…</div>
<script>
const logEl = document.getElementById('log');
const statusEl = document.getElementById('status');
let lastLen = 0;
let busy = false;
async function poll() {
  if (busy) { setTimeout(poll, 2000); return; }
  busy = true;
  try {
    const r = await fetch('/log?t=' + Date.now(), { cache: 'no-store' });
    const t = await r.text();
    if (t.length > lastLen) {
      const atBottom = (window.innerHeight + window.scrollY) >= document.body.scrollHeight - 20;
      logEl.textContent = t;
      lastLen = t.length;
      if (atBottom) window.scrollTo(0, document.body.scrollHeight);
    }
    statusEl.textContent = 'live · ' + new Date().toLocaleTimeString();
  } catch (e) {
    statusEl.textContent = 'disconnected';
  } finally {
    busy = false;
    setTimeout(poll, 2000);
  }
}
poll();
</script>
</body>
</html>`

// logViewerURL points at the /log/view page; empty when the HTTP server
// failed to start so openLogInBrowser falls through silently.
var logViewerURL string

// initLogViewerServer binds config.LogViewerPort then serves the mux
// that backs logViewerHandler. Binding first surfaces port conflicts
// immediately to the caller.
func initLogViewerServer() error {
	addr := fmt.Sprintf("127.0.0.1:%d", config.LogViewerPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	http.HandleFunc("/", logViewerHandler)
	go func() {
		if err := http.Serve(listener, nil); err != nil {
			log.Printf("log viewer server stopped: %v", err)
		}
	}()
	return nil
}

// logViewerHandler routes /log to plain text and /log/view to the HTML
// page; both responses are no-cache and any other path is 404.
func logViewerHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	switch r.URL.Path {
	case "/log":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, serverLog.String())
	case "/log/view":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, logViewerHTML)
	default:
		http.NotFound(w, r)
	}
}

// openLogInBrowser dispatches the user's default browser via cmd /c
// start, which reuses an existing browser instance when possible so
// repeated opens become new tabs rather than new windows.
func openLogInBrowser(url string) {
	if url == "" {
		return
	}
	cmd := exec.Command("cmd", "/c", "start", "", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		log.Printf("openLogInBrowser: %v", err)
	}
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
	if config.ModelsMax <= 0 {
		config.ModelsMax = 1
	}
	if config.LogViewerPort <= 0 {
		config.LogViewerPort = 19967
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

// stopLlamaServer terminates the running llama-server, returning an
// error if the child fails to exit within the grace period.
func stopLlamaServer() error {
	proc.mu.Lock()
	defer proc.mu.Unlock()
	return proc.stopLocked()
}

// stopLocked kills the tracked child if any. Caller must hold proc.mu.
// Graceful taskkill first, escalate to /f /t after 1s, give the Wait
// goroutine up to 5s more, then error.
func (p *llmProcess) stopLocked() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	pid := p.cmd.Process.Pid
	kill := func(force bool) error {
		args := []string{"/pid", strconv.Itoa(pid)}
		if force {
			args = append(args, "/f", "/t")
		}
		return exec.Command("taskkill", args...).Run()
	}
	_ = kill(false)
	select {
	case <-p.done:
	case <-time.After(1 * time.Second):
		if err := kill(true); err != nil {
			log.Printf("taskkill /f failed for pid %d: %v", pid, err)
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("llama-server pid %d did not exit within 6s", pid)
		}
	}
	p.cmd = nil
	p.done = nil
	return nil
}

// startLocked spawns a fresh llama-server child. Caller must hold
// proc.mu and must have already called stopLocked if a previous child
// exists. Each child gets its own done channel closed by its Wait.
func (p *llmProcess) startLocked() error {
	args := []string{
		"--models-preset", "models.ini",
		"--port", strconv.Itoa(config.Port),
		"--host", "0.0.0.0",
		"--models-max", strconv.Itoa(config.ModelsMax),
	}

	serverExe := filepath.Join("server", "llama-server.exe")
	cmd := exec.Command(serverExe, args...)
	cmd.Stdout = &priorityWriter{primary: os.Stdout, secondary: serverLog}
	cmd.Stderr = &priorityWriter{primary: os.Stderr, secondary: serverLog}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start llama-server: %v", err)
	}

	p.cmd = cmd
	p.done = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()

	return waitForServer(config.Port, 60*time.Second)
}

// startLlamaServer stops any existing child and starts a fresh llama-server.
func startLlamaServer() error {
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if err := proc.stopLocked(); err != nil {
		return err
	}
	return proc.startLocked()
}

// refreshLlamaServer resets the log buffer, stops the running child,
// reloads config.json, and starts a fresh llama-server so edits the
// user made to models.ini take effect on return.
func refreshLlamaServer() error {
	serverLog.Reset()
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if err := proc.stopLocked(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if err := loadConfig(); err != nil {
		log.Printf("config reload failed: %v", err)
	}
	return proc.startLocked()
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
	menuItems.refresh = systray.AddMenuItem("Refresh", "Reload models.ini and restart llama-server")
	menuItems.viewLog = systray.AddMenuItem("View Log", "Show llama-server output log")
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
		for range menuItems.refresh.ClickedCh {
			menuItems.refresh.SetTitle("Refreshing…")
			menuItems.refresh.Disable()
			log.Printf("Refreshing llama-server…")
			if err := refreshLlamaServer(); err != nil {
				log.Printf("Refresh failed: %v", err)
				menuItems.refresh.SetTitle("Refresh (failed)")
				openLogInBrowser(logViewerURL)
				time.Sleep(2 * time.Second)
			}
			menuItems.refresh.SetTitle("Refresh")
			menuItems.refresh.Enable()
		}
	}()

	go func() {
		for range menuItems.viewLog.ClickedCh {
			openLogInBrowser(logViewerURL)
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
	if err := stopLlamaServer(); err != nil {
		log.Printf("shutdown failed: %v", err)
	}
}
