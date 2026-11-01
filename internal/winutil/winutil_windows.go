//go:build windows

package winutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	shell32                      = windows.NewLazySystemDLL("shell32.dll")
	procGetUserDefaultUILanguage = kernel32.NewProc("GetUserDefaultUILanguage")
	procGetConsoleProcessList    = kernel32.NewProc("GetConsoleProcessList")
	procShellExecuteExW          = shell32.NewProc("ShellExecuteExW")
)

// Protect encrypts data with DPAPI in machine scope so that the LocalSystem
// service can decrypt what the interactive user stored.
func Protect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("nothing to protect")
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	desc, _ := windows.UTF16PtrFromString("SCLogSync token")
	err := windows.CryptProtectData(&in, desc, nil, 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN|windows.CRYPTPROTECT_LOCAL_MACHINE, &out)
	if err != nil {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// Unprotect reverses Protect.
func Unprotect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("nothing to unprotect")
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	if err != nil {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// ProtectScheme names the encoding stored in config.json.
const ProtectScheme = "dpapi"

// Mutex is a held single-instance lock.
type Mutex struct{ h windows.Handle }

// AcquireMutex takes the global named mutex or returns ErrAlreadyRunning.
func AcquireMutex() (*Mutex, error) {
	name, _ := windows.UTF16PtrFromString(MutexName)
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if h != 0 {
			windows.CloseHandle(h)
		}
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	// Ownership is thread-affine and goroutines migrate between threads, so the
	// lock is "the named object exists": we keep the handle open until Release.
	return &Mutex{h: h}, nil
}

func (m *Mutex) Release() {
	if m == nil || m.h == 0 {
		return
	}
	windows.CloseHandle(m.h)
	m.h = 0
}

// IsElevated reports whether the process runs with an elevated (admin) token.
func IsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400
	swShowNormal          = 1
)

// RunElevated relaunches the current executable with the UAC "runas" verb and
// waits for it. Returns the child's exit code.
func RunElevated(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, err
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = windows.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	dir, _ := windows.UTF16PtrFromString(filepath.Dir(exe))
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		lpDirectory:  dir,
		nShow:        swShowNormal,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	r, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) {
			return -1, ErrElevationCancelled
		}
		return -1, fmt.Errorf("ShellExecuteEx: %w", callErr)
	}
	if info.hProcess == 0 {
		return -1, errors.New("ShellExecuteEx returned no process handle")
	}
	defer windows.CloseHandle(info.hProcess)
	if _, err := windows.WaitForSingleObject(info.hProcess, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return -1, err
	}
	return int(code), nil
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// uiLanguages maps Windows primary language ids to console languages.
var uiLanguages = map[uint16]string{
	0x1f: "tr", 0x07: "de", 0x0c: "fr", 0x0a: "es", 0x10: "it", 0x16: "pt",
	0x15: "pl", 0x19: "ru", 0x04: "zh", 0x11: "ja", 0x12: "ko",
}

// UILanguage returns the console language matching the Windows UI language, else "en".
func UILanguage() string {
	r, _, _ := procGetUserDefaultUILanguage.Call()
	if l, ok := uiLanguages[uint16(r)&0x3ff]; ok {
		return l
	}
	return "en"
}

// EnableConsole switches the console to UTF-8 output and ANSI escape
// processing. Returns whether ANSI colors can be used.
func EnableConsole() bool {
	_ = windows.SetConsoleOutputCP(65001)
	h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}

// OwnsConsole reports whether this process is the only one attached to its
// console, i.e. it was started by double-click (window closes on exit).
func OwnsConsole() bool {
	var pids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// DataDir is %ProgramData%\SCLogSync.
func DataDir() string {
	if d := os.Getenv("SCLOGSYNC_HOME"); d != "" {
		return d
	}
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil || base == "" {
		base = os.Getenv("ProgramData")
	}
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "SCLogSync")
}

// InstallDir is %ProgramFiles%\SCLogSync.
func InstallDir() string {
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil || base == "" {
		base = os.Getenv("ProgramFiles")
	}
	if base == "" {
		base = `C:\Program Files`
	}
	return filepath.Join(base, "SCLogSync")
}

// ExeName is the installed executable name.
const ExeName = "sclogsync.exe"

// RestrictDir sets a protected DACL on dir: SYSTEM, Administrators and the
// current user get full control (inherited by files); everyone else nothing.
func RestrictDir(dir string) error {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sddl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + tu.User.Sid.String() + ")"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// DriveRoots lists existing fixed drive roots ("C:\", "D:\", ...), C..Z.
func DriveRoots() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return []string{`C:\`}
	}
	var out []string
	for i := 2; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := windows.UTF16PtrFromString(root)
		t := windows.GetDriveType(p)
		if t == windows.DRIVE_FIXED || t == windows.DRIVE_REMOVABLE {
			out = append(out, root)
		}
	}
	return out
}

// ComputerName returns the NetBIOS computer name.
func ComputerName() string {
	if n := os.Getenv("COMPUTERNAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

// IsWindows is true on Windows builds.
const IsWindows = true
