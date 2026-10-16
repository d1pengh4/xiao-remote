//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var errRunning = errors.New("receiver already running")
var kernel = syscall.NewLazyDLL("kernel32.dll")
var createMutex = kernel.NewProc("CreateMutexW")
var openMutex = kernel.NewProc("OpenMutexW")
var moveFile = kernel.NewProc("MoveFileExW")

func utf16(s string) *uint16    { p, _ := syscall.UTF16PtrFromString(s); return p }
func samePath(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }
func mutexName(root string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(root))))
	return `Local\XiaoRemoteNative-` + hex.EncodeToString(sum[:8])
}
func acquireInstance(root string) (func(), error) {
	h, _, e := createMutex.Call(0, 1, uintptr(unsafe.Pointer(utf16(mutexName(root)))))
	if h == 0 {
		return nil, errors.New("cannot create receiver lock")
	}
	if e == syscall.ERROR_ALREADY_EXISTS {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, errRunning
	}
	return func() { syscall.CloseHandle(syscall.Handle(h)) }, nil
}
func instanceRunning(root string) bool {
	h, _, _ := openMutex.Call(0x100000, 0, uintptr(unsafe.Pointer(utf16(mutexName(root)))))
	if h == 0 {
		return false
	}
	syscall.CloseHandle(syscall.Handle(h))
	return true
}
func replaceFile(source, target string) error {
	r, _, e := moveFile.Call(uintptr(unsafe.Pointer(utf16(source))), uintptr(unsafe.Pointer(utf16(target))), 0x1|0x8)
	if r == 0 {
		return e
	}
	return nil
}
func consoleAttached() bool { h, _, _ := kernel.NewProc("GetConsoleWindow").Call(); return h != 0 }
func backgroundCommand(path string, args ...string) *exec.Cmd {
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd
}
func startDetached(path string, args ...string) error {
	cmd := backgroundCommand(path, args...)
	if e := cmd.Start(); e != nil {
		return errors.New("cannot start receiver process")
	}
	return cmd.Process.Release()
}
func findBrowser() (string, error) {
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
		if base == "" {
			continue
		}
		for _, suffix := range []string{`Google\Chrome\Application\chrome.exe`, `Microsoft\Edge\Application\msedge.exe`} {
			p := filepath.Join(base, suffix)
			if s, e := os.Stat(p); e == nil && !s.IsDir() {
				return p, nil
			}
		}
	}
	return "", errors.New("install Chrome or Edge")
}
func openBrowser(browser, video, profile string) error {
	if !videoPattern.MatchString(video) {
		return errors.New("invalid video")
	}
	if e := os.MkdirAll(profile, 0700); e != nil {
		return e
	}
	return startDetached(browser, "--user-data-dir="+profile, "--autoplay-policy=no-user-gesture-required", "--no-first-run", "--no-default-browser-check", "--new-window", "https://www.youtube.com/watch?v="+video+"&autoplay=1")
}
func openStatus(path string) error {
	return startDetached(filepath.Join(os.Getenv("WINDIR"), "System32", "notepad.exe"), path)
}
func notify(title, message string) {
	if os.Getenv("XIAO_TEST_NO_UI") == "1" {
		return
	}
	syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(utf16(message))), uintptr(unsafe.Pointer(utf16(title))), 0x10)
}
func parseWindowsCommandLine(line string) []string {
	var count int32
	address, _, _ := syscall.NewLazyDLL("shell32.dll").NewProc("CommandLineToArgvW").Call(uintptr(unsafe.Pointer(utf16(line))), uintptr(unsafe.Pointer(&count)))
	if address == 0 || count < 0 || count > 4096 {
		return nil
	}
	defer kernel.NewProc("LocalFree").Call(address)
	pointers := unsafe.Slice((**uint16)(unsafe.Pointer(address)), int(count))
	result := make([]string, 0, count)
	for _, pointer := range pointers {
		n := 0
		for *(*uint16)(unsafe.Add(unsafe.Pointer(pointer), n*2)) != 0 {
			n++
		}
		result = append(result, syscall.UTF16ToString(unsafe.Slice(pointer, n)))
	}
	return result
}

type processInfo struct {
	ID          uint32 `json:"ProcessId"`
	Name        string `json:"Name"`
	CommandLine string `json:"CommandLine"`
}

func ownedProcess(process processInfo, p paths, closeBrowsers bool) bool {
	args := parseWindowsCommandLine(process.CommandLine)
	if strings.EqualFold(process.Name, "powershell.exe") || strings.EqualFold(process.Name, "pwsh.exe") {
		for i := 1; i+1 < len(args); i++ {
			if strings.EqualFold(args[i], "-File") && samePath(args[i+1], filepath.Join(p.runtime, "receiver.ps1")) {
				return true
			}
		}
	}
	if closeBrowsers && (strings.EqualFold(process.Name, "chrome.exe") || strings.EqualFold(process.Name, "msedge.exe")) {
		for _, arg := range args {
			if strings.HasPrefix(strings.ToLower(arg), "--user-data-dir=") {
				profile := arg[len("--user-data-dir="):]
				parent := filepath.Dir(profile)
				if samePath(filepath.Dir(parent), p.root) && topicPattern.MatchString(filepath.Base(parent)) && samePath(filepath.Base(profile), "BrowserProfile") {
					return true
				}
			}
		}
	}
	return false
}
func stopLegacy(p paths, closeBrowsers bool) error {
	_, legacyErr := os.Stat(filepath.Join(p.runtime, "receiver.ps1"))
	if os.IsNotExist(legacyErr) && !closeBrowsers {
		return nil
	}
	// Query only. Termination is filtered by the exact -File or isolated --user-data-dir argument below.
	script := `ConvertTo-Json -InputObject @(Get-CimInstance Win32_Process -Filter "Name='powershell.exe' OR Name='pwsh.exe' OR Name='chrome.exe' OR Name='msedge.exe'" | Select-Object ProcessId,Name,CommandLine) -Compress`
	helper := backgroundCommand(filepath.Join(os.Getenv("WINDIR"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	data, e := helper.Output()
	if e != nil {
		return errors.New("cannot inspect the former receiver; close it and retry")
	}
	var processes []processInfo
	if e = json.Unmarshal(data, &processes); e != nil {
		return errors.New("cannot inspect receiver processes")
	}
	for _, process := range processes {
		if process.ID == uint32(os.Getpid()) || !ownedProcess(process, p, closeBrowsers) {
			continue
		}
		handle, e := syscall.OpenProcess(0x0001|0x00100000, false, process.ID)
		if e != nil {
			return errors.New("cannot stop an application process")
		}
		e = syscall.TerminateProcess(handle, 0)
		if e == nil {
			_, _ = syscall.WaitForSingleObject(handle, 5000)
		}
		syscall.CloseHandle(handle)
		if e != nil {
			return errors.New("cannot stop an application process")
		}
	}
	return nil
}
func scheduleSelfRemoval() {
	current, e := os.Executable()
	if e != nil {
		return
	}
	directory := filepath.Dir(current)
	if !strings.HasPrefix(filepath.Base(directory), "xiao-uninstall-") {
		return
	}
	// A standard uninstaller helper waits for this executable to exit before removing it.
	// LiteralPath and PowerShell string quoting handle spaces, apostrophes and shell metacharacters.
	command := "Wait-Process -Id " + strconv.Itoa(os.Getpid()) + " -ErrorAction SilentlyContinue; Remove-Item -LiteralPath '" + strings.ReplaceAll(directory, "'", "''") + "' -Recurse -Force"
	helper := backgroundCommand(filepath.Join(os.Getenv("WINDIR"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command)
	if helper.Start() == nil {
		_ = helper.Process.Release()
	}
}
