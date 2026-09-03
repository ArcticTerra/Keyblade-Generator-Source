//go:build windows

package main

import (
	"archive/zip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const appVersion = "1.1.8"

// KH1FM btltbl.bin weapon table layout validated in-game during development.
const (
	btlTableBase  = 0x94F0
	btlRecordSize = 0x58
	offReach      = 0x2C
	offStrength   = 0x38
	offCritRate   = 0x39
	offCritBonus  = 0x3A
	offRecoil     = 0x3B
	offMP         = 0x40
)

var sharedSCDs = map[string]bool{
	"se011018.win32.scd": true,
	"se011019.win32.scd": true,
	"se011020.win32.scd": true,
	"se011021.win32.scd": true,
	"se011022.win32.scd": true,
}

type Keyblade struct {
	ID                  string                   `json:"id"`
	Name                string                   `json:"name"`
	Stem                string                   `json:"stem,omitempty"`
	RecordIndex         int                      `json:"recordIndex,omitempty"`
	Builtin             bool                     `json:"builtin"`
	Targetable          bool                     `json:"targetable"`
	RawWpn              string                   `json:"rawWpn,omitempty"`
	SoundFolder         string                   `json:"soundFolder,omitempty"`
	AudioSourceID       string                   `json:"audioSourceId,omitempty"`
	GameRawWpn          string                   `json:"gameRawWpn,omitempty"`
	GameSoundFolder     string                   `json:"gameSoundFolder,omitempty"`
	GameplayRecordIndex *int                     `json:"gameplayRecordIndex,omitempty"`
	Stats               *ExternalStats           `json:"stats,omitempty"`
	RemasteredFiles     []ExternalRemasteredFile `json:"remasteredFiles,omitempty"`
	CopyRawWpn          bool                     `json:"copyRawWpn,omitempty"`
	PackRoot            string                   `json:"-"`
}

type ExternalRemasteredFile struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type ExternalStats struct {
	Reach *float32 `json:"reach,omitempty"`
}

type DefinitionFile struct {
	SchemaVersion int        `json:"schemaVersion"`
	Keyblades     []Keyblade `json:"keyblades"`
}

type Settings struct {
	OpenKhRoot      string `json:"OpenKhRoot"`
	GameDt          string `json:"GameDt"`
	PatchManagerExe string `json:"PatchManagerExe"`
	LastOutputPath  string `json:"LastOutputPath,omitempty"` // legacy v1.0.5 setting
	BuildOutputPath string `json:"BuildOutputPath,omitempty"`
	CustomBuildPath bool   `json:"CustomBuildPath,omitempty"`
	DefaultModName  string `json:"DefaultModName"`
	DefaultAuthor   string `json:"DefaultAuthor"`
}

type WeaponStats struct {
	Strength    int
	MP          int
	CritRateRaw int
	CritBonus   int
	Recoil      int
	Reach       float32
}

type QueueItem struct {
	TargetID    string      `json:"TargetId"`
	SourceID    string      `json:"SourceId"`
	MatchLength bool        `json:"MatchLength"`
	CustomStats bool        `json:"CustomStats"`
	Stats       WeaponStats `json:"Stats"`
}

type Manifest struct {
	GeneratorVersion string      `json:"generatorVersion"`
	ModName          string      `json:"modName"`
	Author           string      `json:"author"`
	GeneratedAt      string      `json:"generatedAt"`
	Replacements     []QueueItem `json:"replacements"`
}

var (
	appRoot   string
	settings  Settings
	keyblades []Keyblade
	targets   []Keyblade
	sources   []Keyblade
	rawIndex  map[string]string
	baseBtl   []byte
	queue     []QueueItem

	hInstance syscall.Handle
	mainWnd   syscall.Handle
	font      syscall.Handle

	// controls
	targetButton, sourceButton                                               syscall.Handle
	targetSel, sourceSel                                                     int
	targetInfo, sourceStatus                                                 syscall.Handle
	chkMatch, chkCustom                                                      syscall.Handle
	editStrength, editMP, editCritRate, editCritBonus, editRecoil, editReach syscall.Handle
	btnAdd, listQueue, btnRemove, btnClear, btnDeselect, btnGenerate         syscall.Handle
	btnSetup, btnPrepare, btnOpenLog, btnChangeSave                          syscall.Handle
	editModName, editAuthor                                                  syscall.Handle
	lblConfig, lblReplacementList, lblModName, lblAuthor, lblFooter          syscall.Handle
	lblReachHint, lblSavePath, lblStatHelp, lblBusyStatus                    syscall.Handle
	queueRowMap                                                              []int
	baseClientW, baseClientH                                                 int32
	currentDPI                                                               uint32 = 96
	controlLayouts                                                                  = map[syscall.Handle]controlLayout{}
	checkStates                                                                     = map[syscall.Handle]bool{}
	checkLabels                                                                     = map[syscall.Handle]string{}
	statHelpByHandle                                                                = map[syscall.Handle]string{}
	currentStatHelp                                                          string
	statHelpVisible                                                          bool
	tabOrder                                                                 []syscall.Handle

	sessionLogPath        string
	lastCrashPath         string
	previousCrashDetected bool
)

func logDirPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = appRoot
	}
	return filepath.Join(base, "KH1KeybladeGenerator", "logs")
}

func logEvent(format string, args ...interface{}) {
	if sessionLogPath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(sessionLogPath), 0755)
	f, err := os.OpenFile(sessionLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	line := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(f, "%s  %s\r\n", time.Now().Format("2006-01-02 15:04:05.000"), line)
}

func initCrashLogging() {
	dir := logDirPath()
	_ = os.MkdirAll(dir, 0755)
	sessionLogPath = filepath.Join(dir, "current_session.log")
	if old, err := os.ReadFile(sessionLogPath); err == nil && len(old) > 0 && !strings.Contains(string(old), "[CLEAN EXIT]") {
		stamp := time.Now().Format("20060102_150405")
		lastCrashPath = filepath.Join(dir, "Crash_"+stamp+".txt")
		_ = os.WriteFile(lastCrashPath, old, 0644)
		previousCrashDetected = true
	}
	_ = os.WriteFile(sessionLogPath, nil, 0644)
	logEvent("KH1 Keyblade Generator v%s session started", appVersion)
	logEvent("Go runtime: %s | GOOS=%s GOARCH=%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	logEvent("Executable root: %s", appRoot)
}

func markCleanExit() { logEvent("[CLEAN EXIT]") }

func copyCurrentLogToCrashReport(reason string) string {
	logEvent("FATAL: %s", reason)
	dir := logDirPath()
	stamp := time.Now().Format("20060102_150405")
	p := filepath.Join(dir, "Crash_"+stamp+".txt")
	if b, err := os.ReadFile(sessionLogPath); err == nil {
		_ = os.WriteFile(p, b, 0644)
	}
	lastCrashPath = p
	return p
}

func reportRecoveredPanic(where string, recovered interface{}) {
	logEvent("PANIC in %s: %v", where, recovered)
	logEvent("STACK TRACE:\r\n%s", string(debug.Stack()))
	p := copyCurrentLogToCrashReport(fmt.Sprintf("panic in %s: %v", where, recovered))
	messageBox(mainWnd, "The generator intercepted a crash instead of disappearing.\r\n\r\nA detailed report was saved here:\r\n"+p+"\r\n\r\nReopen the generator and click 'Open Last Crash Report', then copy/paste the report into ChatGPT.", "KH1 Keyblade Generator - Crash Report", MB_OK|MB_ICONERROR)
}

// Win32 API
var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")
	comdlg32 = syscall.NewLazyDLL("comdlg32.dll")

	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procUpdateWindow                  = user32.NewProc("UpdateWindow")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procTranslateMessage              = user32.NewProc("TranslateMessage")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procPostQuitMessage               = user32.NewProc("PostQuitMessage")
	procSendMessageW                  = user32.NewProc("SendMessageW")
	procMessageBoxW                   = user32.NewProc("MessageBoxW")
	procSetWindowTextW                = user32.NewProc("SetWindowTextW")
	procGetWindowTextW                = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW          = user32.NewProc("GetWindowTextLengthW")
	procEnableWindow                  = user32.NewProc("EnableWindow")
	procDestroyWindow                 = user32.NewProc("DestroyWindow")
	procLoadIconW                     = user32.NewProc("LoadIconW")
	procLoadCursorW                   = user32.NewProc("LoadCursorW")
	procCreatePopupMenu               = user32.NewProc("CreatePopupMenu")
	procAppendMenuW                   = user32.NewProc("AppendMenuW")
	procTrackPopupMenu                = user32.NewProc("TrackPopupMenu")
	procDestroyMenu                   = user32.NewProc("DestroyMenu")
	procGetWindowRect                 = user32.NewProc("GetWindowRect")
	procGetClientRect                 = user32.NewProc("GetClientRect")
	procMoveWindow                    = user32.NewProc("MoveWindow")
	procSetWindowPos                  = user32.NewProc("SetWindowPos")
	procInvalidateRect                = user32.NewProc("InvalidateRect")
	procDrawFrameControl              = user32.NewProc("DrawFrameControl")
	procDrawTextW                     = user32.NewProc("DrawTextW")
	procFillRect                      = user32.NewProc("FillRect")
	procGetSysColorBrush              = user32.NewProc("GetSysColorBrush")
	procSetTimer                      = user32.NewProc("SetTimer")
	procKillTimer                     = user32.NewProc("KillTimer")
	procGetCursorPos                  = user32.NewProc("GetCursorPos")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procGetDpiForSystem               = user32.NewProc("GetDpiForSystem")
	procSetFocus                      = user32.NewProc("SetFocus")
	procGetKeyState                   = user32.NewProc("GetKeyState")
	procIsWindowEnabled               = user32.NewProc("IsWindowEnabled")
	procIsWindowVisible               = user32.NewProc("IsWindowVisible")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procCreateFontW      = gdi32.NewProc("CreateFontW")
	procDeleteObject     = gdi32.NewProc("DeleteObject")
	procSelectObject     = gdi32.NewProc("SelectObject")
	procSetBkMode        = gdi32.NewProc("SetBkMode")
	procSetTextColor     = gdi32.NewProc("SetTextColor")

	procSHBrowseForFolderW          = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW        = shell32.NewProc("SHGetPathFromIDListW")
	procSHCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")
	procCoTaskMemFree               = ole32.NewProc("CoTaskMemFree")
	procCoInitializeEx              = ole32.NewProc("CoInitializeEx")
	procCoCreateInstance            = ole32.NewProc("CoCreateInstance")

	procGetOpenFileNameW = comdlg32.NewProc("GetOpenFileNameW")
)

const (
	CS_HREDRAW      = 0x0002
	CS_VREDRAW      = 0x0001
	COLOR_WINDOW    = 5
	IDC_ARROW       = 32512
	IDI_APPLICATION = 32512

	WS_OVERLAPPEDWINDOW = 0x00CF0000
	WS_VISIBLE          = 0x10000000
	WS_CHILD            = 0x40000000
	WS_TABSTOP          = 0x00010000
	WS_BORDER           = 0x00800000
	WS_VSCROLL          = 0x00200000
	WS_EX_CLIENTEDGE    = 0x00000200

	SS_LEFT          = 0x00000000
	BS_PUSHBUTTON    = 0x00000000
	BS_AUTOCHECKBOX  = 0x00000003
	BS_OWNERDRAW     = 0x0000000B
	CBS_DROPDOWNLIST = 0x0003
	LBS_NOTIFY       = 0x0001
	ES_AUTOHSCROLL   = 0x0080

	WM_CREATE         = 0x0001
	WM_DESTROY        = 0x0002
	WM_CLOSE          = 0x0010
	WM_COMMAND        = 0x0111
	WM_SIZE           = 0x0005
	WM_SETFONT        = 0x0030
	WM_DRAWITEM       = 0x002B
	WM_TIMER          = 0x0113
	WM_GETMINMAXINFO  = 0x0024
	WM_CTLCOLORSTATIC = 0x0138
	WM_DPICHANGED     = 0x02E0
	WM_KEYDOWN        = 0x0100

	VK_TAB   = 0x09
	VK_SHIFT = 0x10

	SW_HIDE = 0
	SW_SHOW = 5

	BN_CLICKED    = 0
	CBN_SELCHANGE = 1
	LBN_SELCHANGE = 1

	BM_GETCHECK   = 0x00F0
	BM_SETCHECK   = 0x00F1
	BST_UNCHECKED = 0
	BST_CHECKED   = 1

	CB_ADDSTRING    = 0x0143
	CB_GETCURSEL    = 0x0147
	CB_SETCURSEL    = 0x014E
	CB_RESETCONTENT = 0x014B

	LB_ADDSTRING    = 0x0180
	LB_RESETCONTENT = 0x0184
	LB_GETCURSEL    = 0x0188
	LB_DELETESTRING = 0x0182
	LB_SETCURSEL    = 0x0186

	MB_OK              = 0x00000000
	MB_ICONERROR       = 0x00000010
	MB_ICONINFORMATION = 0x00000040
	MB_ICONQUESTION    = 0x00000020
	MB_YESNO           = 0x00000004
	IDYES              = 6

	MF_STRING       = 0x00000000
	TPM_LEFTALIGN   = 0x0000
	TPM_TOPALIGN    = 0x0000
	TPM_RIGHTBUTTON = 0x0002
	TPM_RETURNCMD   = 0x0100
	TPM_NONOTIFY    = 0x0080

	BIF_RETURNONLYFSDIRS = 0x00000001
	BIF_NEWDIALOGSTYLE   = 0x00000040

	OFN_FILEMUSTEXIST = 0x00001000
	OFN_PATHMUSTEXIST = 0x00000800
	OFN_EXPLORER      = 0x00080000

	COINIT_APARTMENTTHREADED = 0x2
	CLSCTX_INPROC_SERVER     = 0x1

	FOS_NOCHANGEDIR     = 0x00000008
	FOS_PICKFOLDERS     = 0x00000020
	FOS_FORCEFILESYSTEM = 0x00000040
	FOS_PATHMUSTEXIST   = 0x00000800
	SIGDN_FILESYSPATH   = 0x80058000

	EN_CHANGE = 0x0300

	DFC_BUTTON       = 4
	DFCS_BUTTONCHECK = 0x0000
	DFCS_CHECKED     = 0x0400
	DFCS_PUSHED      = 0x0200
	ODS_SELECTED     = 0x0001
	DT_LEFT          = 0x0000
	DT_VCENTER       = 0x0004
	DT_SINGLELINE    = 0x0020
	TRANSPARENT      = 1
)

const (
	idTargetButton = 1001
	idSourceButton = 1002
	idMatch        = 1003
	idCustom       = 1004
	idStrength     = 1010
	idMP           = 1011
	idCritRate     = 1012
	idCritBonus    = 1013
	idRecoil       = 1014
	idReach        = 1015
	idAdd          = 1020
	idQueue        = 1030
	idRemove       = 1031
	idClear        = 1032
	idDeselect     = 1033
	idGenerate     = 1040
	idChangeSave   = 1041
	idSetup        = 1050
	idPrepare      = 1051
	idOpenLog      = 1052
)

type WNDCLASSEX struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     syscall.Handle
	HIcon         syscall.Handle
	HCursor       syscall.Handle
	HbrBackground syscall.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       syscall.Handle
}

type POINT struct{ X, Y int32 }
type RECT struct{ Left, Top, Right, Bottom int32 }
type controlLayout struct{ X, Y, W, H int32 }
type MINMAXINFO struct {
	PtReserved     POINT
	PtMaxSize      POINT
	PtMaxPosition  POINT
	PtMinTrackSize POINT
	PtMaxTrackSize POINT
}
type DRAWITEMSTRUCT struct {
	CtlType    uint32
	CtlID      uint32
	ItemID     uint32
	ItemAction uint32
	ItemState  uint32
	HwndItem   syscall.Handle
	HDC        syscall.Handle
	RcItem     RECT
	ItemData   uintptr
}
type MSG struct {
	HWnd     syscall.Handle
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       POINT
	LPrivate uint32
}

type BROWSEINFO struct {
	HwndOwner      syscall.Handle
	PidlRoot       uintptr
	PszDisplayName *uint16
	LpszTitle      *uint16
	UlFlags        uint32
	Lpfn           uintptr
	LParam         uintptr
	IImage         int32
}

type OPENFILENAME struct {
	LStructSize       uint32
	HwndOwner         syscall.Handle
	HInstance         syscall.Handle
	LpstrFilter       *uint16
	LpstrCustomFilter *uint16
	NMaxCustFilter    uint32
	NFilterIndex      uint32
	LpstrFile         *uint16
	NMaxFile          uint32
	LpstrFileTitle    *uint16
	NMaxFileTitle     uint32
	LpstrInitialDir   *uint16
	LpstrTitle        *uint16
	Flags             uint32
	NFileOffset       uint16
	NFileExtension    uint16
	LpstrDefExt       *uint16
	LcustData         uintptr
	LpfnHook          uintptr
	LpTemplateName    *uint16
	PvReserved        uintptr
	DwReserved        uint32
	FlagsEx           uint32
}

type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidFileOpenDialog = GUID{0xDC1C5A9C, 0xE88A, 0x4DDE, [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = GUID{0xD57C7288, 0xD4AD, 0x4768, [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	iidIShellItem       = GUID{0x43826D1E, 0xE718, 0x42EE, [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
)

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func loword(v uintptr) uint16 { return uint16(v & 0xffff) }
func hiword(v uintptr) uint16 { return uint16((v >> 16) & 0xffff) }

func messageBox(owner syscall.Handle, text, title string, flags uintptr) int {
	textPtr, _ := syscall.UTF16PtrFromString(text)
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	r, _, _ := procMessageBoxW.Call(uintptr(owner), uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(titlePtr)), flags)
	runtime.KeepAlive(textPtr)
	runtime.KeepAlive(titlePtr)
	return int(r)
}
func showError(msg string) { messageBox(mainWnd, msg, "KH1 Keyblade Generator", MB_OK|MB_ICONERROR) }
func showInfo(msg string) {
	messageBox(mainWnd, msg, "KH1 Keyblade Generator", MB_OK|MB_ICONINFORMATION)
}
func askYesNo(msg, title string) bool {
	return messageBox(mainWnd, msg, title, MB_YESNO|MB_ICONQUESTION) == IDYES
}

func setText(hwnd syscall.Handle, s string) {
	p, _ := syscall.UTF16PtrFromString(s)
	procSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
}
func getText(hwnd syscall.Handle) string {
	n, _, _ := procGetWindowTextLengthW.Call(uintptr(hwnd))
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), n+1)
	return syscall.UTF16ToString(buf)
}
func enable(hwnd syscall.Handle, yes bool) {
	v := uintptr(0)
	if yes {
		v = 1
	}
	procEnableWindow.Call(uintptr(hwnd), v)
}
func scalePx(v int32) int32 {
	if currentDPI == 0 {
		currentDPI = 96
	}
	return int32((int64(v)*int64(currentDPI) + 48) / 96)
}
func unscalePx(v int32) int32 {
	if currentDPI == 0 {
		return v
	}
	return int32((int64(v)*96 + int64(currentDPI)/2) / int64(currentDPI))
}
func moveControlRaw(hwnd syscall.Handle, x, y, w, h int32) {
	if hwnd == 0 {
		return
	}
	procMoveWindow.Call(uintptr(hwnd), uintptr(x), uintptr(y), uintptr(w), uintptr(h), 1)
}
func moveControl(hwnd syscall.Handle, x, y, w, h int32) {
	if hwnd == 0 {
		return
	}
	controlLayouts[hwnd] = controlLayout{x, y, w, h}
	moveControlRaw(hwnd, scalePx(x), scalePx(y), scalePx(w), scalePx(h))
}
func rescaleAllControls() {
	for hwnd, r := range controlLayouts {
		moveControlRaw(hwnd, scalePx(r.X), scalePx(r.Y), scalePx(r.W), scalePx(r.H))
	}
}
func send(hwnd syscall.Handle, msg uint32, wp, lp uintptr) uintptr {
	r, _, _ := procSendMessageW.Call(uintptr(hwnd), uintptr(msg), wp, lp)
	return r
}
func isChecked(hwnd syscall.Handle) bool {
	if v, ok := checkStates[hwnd]; ok {
		return v
	}
	return send(hwnd, BM_GETCHECK, 0, 0) == BST_CHECKED
}
func setChecked(hwnd syscall.Handle, yes bool) {
	if _, ok := checkStates[hwnd]; ok {
		checkStates[hwnd] = yes
		procInvalidateRect.Call(uintptr(hwnd), 0, 1)
		return
	}
	v := uintptr(BST_UNCHECKED)
	if yes {
		v = BST_CHECKED
	}
	send(hwnd, BM_SETCHECK, v, 0)
}
func toggleChecked(hwnd syscall.Handle) { setChecked(hwnd, !isChecked(hwnd)) }

func isWindowEnabled(hwnd syscall.Handle) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := procIsWindowEnabled.Call(uintptr(hwnd))
	return r != 0
}

func isWindowVisible(hwnd syscall.Handle) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
	return r != 0
}

func handleTabNavigation(msg *MSG) bool {
	if msg == nil || msg.Message != WM_KEYDOWN || msg.WParam != VK_TAB || len(tabOrder) == 0 {
		return false
	}
	current := msg.HWnd
	idx := -1
	for i, h := range tabOrder {
		if h == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}

	state, _, _ := procGetKeyState.Call(VK_SHIFT)
	reverse := state&0x8000 != 0
	step := 1
	if reverse {
		step = -1
	}

	for n := 1; n <= len(tabOrder); n++ {
		next := (idx + step*n) % len(tabOrder)
		if next < 0 {
			next += len(tabOrder)
		}
		h := tabOrder[next]
		if isWindowEnabled(h) && isWindowVisible(h) {
			procSetFocus.Call(uintptr(h))
			return true
		}
	}
	return true
}

func createControl(exStyle uint32, class, text string, style uint32, x, y, w, h int32, parent syscall.Handle, id int) syscall.Handle {
	classPtr, _ := syscall.UTF16PtrFromString(class)
	textPtr, _ := syscall.UTF16PtrFromString(text)
	r, _, _ := procCreateWindowExW.Call(
		uintptr(exStyle), uintptr(unsafe.Pointer(classPtr)), uintptr(unsafe.Pointer(textPtr)), uintptr(style),
		uintptr(scalePx(x)), uintptr(scalePx(y)), uintptr(scalePx(w)), uintptr(scalePx(h)), uintptr(parent), uintptr(id), uintptr(hInstance), 0,
	)
	runtime.KeepAlive(classPtr)
	runtime.KeepAlive(textPtr)
	hwnd := syscall.Handle(r)
	if hwnd != 0 {
		controlLayouts[hwnd] = controlLayout{x, y, w, h}
	}
	if font != 0 {
		send(hwnd, WM_SETFONT, uintptr(font), 1)
	}
	return hwnd
}

func addLabel(text string, x, y, w, h int32) syscall.Handle {
	return createControl(0, "STATIC", text, WS_CHILD|WS_VISIBLE|SS_LEFT, x, y, w, h, mainWnd, 0)
}
func addButton(id int, text string, x, y, w, h int32) syscall.Handle {
	return createControl(0, "BUTTON", text, WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON, x, y, w, h, mainWnd, id)
}
func addCheck(id int, text string, x, y, w, h int32) syscall.Handle {
	hwnd := createControl(0, "BUTTON", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_OWNERDRAW, x, y, w, h, mainWnd, id)
	if hwnd != 0 {
		checkStates[hwnd] = false
		checkLabels[hwnd] = text
	}
	return hwnd
}
func addEdit(id int, x, y, w, h int32) syscall.Handle {
	return createControl(WS_EX_CLIENTEDGE, "EDIT", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|ES_AUTOHSCROLL, x, y, w, h, mainWnd, id)
}

func drawLargeCheckbox(dis *DRAWITEMSTRUCT) {
	if dis == nil || dis.HwndItem == 0 || dis.HDC == 0 {
		return
	}
	brush, _, _ := procGetSysColorBrush.Call(COLOR_WINDOW)
	procFillRect.Call(uintptr(dis.HDC), uintptr(unsafe.Pointer(&dis.RcItem)), brush)

	box := scalePx(22)
	left := dis.RcItem.Left + scalePx(2)
	top := dis.RcItem.Top + (dis.RcItem.Bottom-dis.RcItem.Top-box)/2
	boxRect := RECT{Left: left, Top: top, Right: left + box, Bottom: top + box}
	state := uintptr(DFCS_BUTTONCHECK)
	if isChecked(dis.HwndItem) {
		state |= DFCS_CHECKED
	}
	if dis.ItemState&ODS_SELECTED != 0 {
		state |= DFCS_PUSHED
	}
	procDrawFrameControl.Call(uintptr(dis.HDC), uintptr(unsafe.Pointer(&boxRect)), DFC_BUTTON, state)

	label := checkLabels[dis.HwndItem]
	if label != "" {
		textRect := RECT{Left: boxRect.Right + scalePx(9), Top: dis.RcItem.Top, Right: dis.RcItem.Right, Bottom: dis.RcItem.Bottom}
		p, _ := syscall.UTF16PtrFromString(label)
		oldFont, _, _ := procSelectObject.Call(uintptr(dis.HDC), uintptr(font))
		procSetBkMode.Call(uintptr(dis.HDC), TRANSPARENT)
		procDrawTextW.Call(uintptr(dis.HDC), uintptr(unsafe.Pointer(p)), ^uintptr(0), uintptr(unsafe.Pointer(&textRect)), DT_LEFT|DT_VCENTER|DT_SINGLELINE)
		if oldFont != 0 {
			procSelectObject.Call(uintptr(dis.HDC), oldFont)
		}
		runtime.KeepAlive(p)
	}
}

func setBusyStatus(text string) {
	if lblBusyStatus == 0 {
		return
	}
	logEvent("STATUS: %s", text)
	setText(lblBusyStatus, text)
	procShowWindow.Call(uintptr(lblBusyStatus), SW_SHOW)
	procInvalidateRect.Call(uintptr(lblBusyStatus), 0, 1)
	procUpdateWindow.Call(uintptr(lblBusyStatus))
	procUpdateWindow.Call(uintptr(mainWnd))
}

func clearBusyStatus() {
	if lblBusyStatus == 0 {
		return
	}
	setText(lblBusyStatus, "")
	procShowWindow.Call(uintptr(lblBusyStatus), SW_HIDE)
}

func registerStatHelp(help string, handles ...syscall.Handle) {
	for _, h := range handles {
		if h != 0 {
			statHelpByHandle[h] = help
		}
	}
}

func cursorInsideWindow(hwnd syscall.Handle, p POINT) bool {
	if hwnd == 0 {
		return false
	}
	var r RECT
	if ok, _, _ := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r))); ok == 0 {
		return false
	}
	return p.X >= r.Left && p.X < r.Right && p.Y >= r.Top && p.Y < r.Bottom
}

func updateStatHelpHover() {
	if lblStatHelp == 0 {
		return
	}
	var p POINT
	if ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p))); ok == 0 {
		if statHelpVisible {
			procShowWindow.Call(uintptr(lblStatHelp), SW_HIDE)
			statHelpVisible = false
			currentStatHelp = ""
		}
		return
	}

	// Work out which help entry is under the pointer first, then only repaint
	// the panel when the text actually changes. v1.0.7 called SetWindowTextW
	// every 100 ms while hovering, which caused the STATIC control to visibly
	// erase/repaint and look like the help text was flashing.
	newHelp := ""
	for h, help := range statHelpByHandle {
		if cursorInsideWindow(h, p) {
			newHelp = help
			break
		}
	}

	if newHelp != "" {
		if newHelp != currentStatHelp {
			setText(lblStatHelp, newHelp)
			currentStatHelp = newHelp
		}
		if !statHelpVisible {
			procShowWindow.Call(uintptr(lblStatHelp), SW_SHOW)
			statHelpVisible = true
		}
		return
	}

	if statHelpVisible {
		procShowWindow.Call(uintptr(lblStatHelp), SW_HIDE)
		statHelpVisible = false
		currentStatHelp = ""
	}
}

func defaultSettings() Settings {
	s := Settings{OpenKhRoot: `C:\OpenKH Extractions\kh1`, DefaultModName: "Generated Keyblade Replacer", DefaultAuthor: "ArcticTerra"}
	candidates := []string{
		`D:\Steam\steamapps\common\KINGDOM HEARTS -HD 1.5+2.5 ReMIX-\Image\dt`,
		`C:\Program Files (x86)\Steam\steamapps\common\KINGDOM HEARTS -HD 1.5+2.5 ReMIX-\Image\dt`,
		`C:\Program Files\Steam\steamapps\common\KINGDOM HEARTS -HD 1.5+2.5 ReMIX-\Image\dt`,
	}
	for _, p := range candidates {
		if dirExists(p) {
			s.GameDt = p
			break
		}
	}
	return s
}

func settingsPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = appRoot
	}
	return filepath.Join(base, "KH1KeybladeGenerator", "settings.json")
}
func loadSettings() {
	settings = defaultSettings()
	b, err := os.ReadFile(settingsPath())
	if err == nil {
		_ = json.Unmarshal(b, &settings)
	}
	if settings.DefaultModName == "" {
		settings.DefaultModName = "Generated Keyblade Replacer"
	}
	if settings.DefaultAuthor == "" {
		settings.DefaultAuthor = "ArcticTerra"
	}
}
func saveSettings() {
	p := settingsPath()
	_ = os.MkdirAll(filepath.Dir(p), 0755)
	b, _ := json.MarshalIndent(settings, "", "  ")
	_ = os.WriteFile(p, b, 0644)
}

func defaultBuildOutputPath() string { return filepath.Join(appRoot, "Built Mods") }
func buildOutputPath() string {
	if settings.CustomBuildPath && strings.TrimSpace(settings.BuildOutputPath) != "" {
		return settings.BuildOutputPath
	}
	return defaultBuildOutputPath()
}
func updateSavePathLabel() {
	if lblSavePath == 0 {
		return
	}
	p := buildOutputPath()
	display := p
	if !settings.CustomBuildPath {
		display = "Built Mods (next to the generator)"
	}
	setText(lblSavePath, "Save path: "+display)
}
func changeSavePath() {
	initial := buildOutputPath()
	if !dirExists(initial) {
		_ = os.MkdirAll(initial, 0755)
	}
	if p, ok := browseFolder(mainWnd, "Choose where generated OpenKH mods are saved", initial); ok {
		settings.BuildOutputPath = p
		settings.CustomBuildPath = true
		saveSettings()
		updateSavePathLabel()
		logEvent("Build output path changed: %s", p)
	}
}

func fileExists(p string) bool { st, e := os.Stat(p); return e == nil && !st.IsDir() }
func dirExists(p string) bool  { st, e := os.Stat(p); return e == nil && st.IsDir() }

func loadDefinitions() error {
	keyblades = nil
	builtinPath := filepath.Join(appRoot, "data", "keyblades.json")
	b, err := os.ReadFile(builtinPath)
	if err != nil {
		return fmt.Errorf("missing required data file: %s", builtinPath)
	}
	var df DefinitionFile
	if err = json.Unmarshal(b, &df); err != nil {
		return err
	}
	for i := range df.Keyblades {
		df.Keyblades[i].PackRoot = filepath.Dir(builtinPath)
		keyblades = append(keyblades, df.Keyblades[i])
	}
	packsRoot := filepath.Join(appRoot, "Packs")
	entries, _ := os.ReadDir(packsRoot)
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".json" {
			continue
		}
		p := filepath.Join(packsRoot, e.Name())
		pb, er := os.ReadFile(p)
		if er != nil {
			continue
		}
		var pf DefinitionFile
		if json.Unmarshal(pb, &pf) != nil {
			continue
		}
		for i := range pf.Keyblades {
			pf.Keyblades[i].PackRoot = filepath.Dir(p)
			// external definitions default source-only
			keyblades = append(keyblades, pf.Keyblades[i])
		}
	}
	targets = nil
	sources = nil
	for _, k := range keyblades {
		if k.Targetable {
			targets = append(targets, k)
		}
		sources = append(sources, k)
	}
	if len(targets) == 0 {
		return errors.New("no target Keyblades loaded")
	}
	return nil
}
func getKeyblade(id string) *Keyblade {
	for i := range keyblades {
		if keyblades[i].ID == id {
			return &keyblades[i]
		}
	}
	return nil
}

func assetCacheDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = appRoot
	}
	return filepath.Join(base, "KH1KeybladeGenerator", "assets", "raw")
}

func indexRawDir(rd string, seen map[string]bool) {
	if !dirExists(rd) || seen[rd] {
		return
	}
	seen[rd] = true
	ents, _ := os.ReadDir(rd)
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := strings.ToLower(e.Name())
		if strings.HasSuffix(n, ".wpn") {
			if _, ok := rawIndex[n]; !ok {
				rawIndex[n] = filepath.Join(rd, e.Name())
			}
		}
	}
}

func refreshBaseData() {
	rawIndex = map[string]string{}
	seen := map[string]bool{}

	// Prefer the generator's compact persistent cache. This keeps source assets
	// available even if a large temporary KHPCPatchManager extraction is removed.
	indexRawDir(assetCacheDir(), seen)

	roots := []string{}
	if dirExists(settings.GameDt) {
		roots = append(roots, settings.GameDt)
	}
	if dirExists(settings.OpenKhRoot) {
		roots = append(roots, settings.OpenKhRoot)
	}
	for _, root := range roots {
		if strings.EqualFold(filepath.Base(root), "raw") {
			indexRawDir(root, seen)
		}
		indexRawDir(filepath.Join(root, "raw"), seen)
		ents, _ := os.ReadDir(root)
		for _, e := range ents {
			if e.IsDir() && strings.HasPrefix(strings.ToLower(e.Name()), "kh1_") && strings.HasSuffix(strings.ToLower(e.Name()), ".hed_out") {
				indexRawDir(filepath.Join(root, e.Name(), "raw"), seen)
			}
		}
	}
	baseBtl = nil
	p := filepath.Join(settings.OpenKhRoot, "btltbl.bin")
	if fileExists(p) {
		baseBtl, _ = os.ReadFile(p)
	}
	updateConfigLabel()
}

func cacheExistingRawAssets() (int, error) {
	refreshBaseData()
	cache := assetCacheDir()
	if err := os.MkdirAll(cache, 0755); err != nil {
		return countBuiltinRaw(), err
	}
	copied := 0
	wanted := map[string]bool{}
	for i := range keyblades {
		k := &keyblades[i]
		if k.Builtin && k.Stem != "" {
			wanted["xw_ex_"+k.Stem+".wpn"] = true
		}
		if !k.Builtin && k.GameRawWpn != "" {
			wanted[filepath.Base(k.GameRawWpn)] = true
		}
	}
	for name := range wanted {
		src := rawIndex[strings.ToLower(name)]
		if src == "" {
			continue
		}
		dst := filepath.Join(cache, name)
		if fileExists(dst) {
			continue
		}
		if err := copyFile(src, dst); err != nil {
			return countBuiltinRaw(), err
		}
		copied++
	}
	if copied > 0 {
		logEvent("Cached %d RAW weapon model(s) into %s", copied, cache)
	}
	refreshBaseData()
	return countBuiltinRaw(), nil
}

func resolveRawWpn(k *Keyblade) string {
	if k == nil {
		return ""
	}
	if k.Builtin {
		return rawIndex[strings.ToLower("xw_ex_"+k.Stem+".wpn")]
	}
	if k.RawWpn != "" {
		p := filepath.Join(k.PackRoot, k.RawWpn)
		if fileExists(p) {
			return p
		}
	}
	if k.GameRawWpn != "" {
		return rawIndex[strings.ToLower(filepath.Base(k.GameRawWpn))]
	}
	return ""
}
func validPackRelativePath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || filepath.IsAbs(p) {
		return false
	}
	c := filepath.Clean(p)
	return c != "." && c != ".." && !strings.HasPrefix(c, ".."+string(os.PathSeparator))
}
func resolveRemasteredFile(k *Keyblade, f ExternalRemasteredFile) string {
	if k == nil || !validPackRelativePath(f.Source) || !validPackRelativePath(f.Target) {
		return ""
	}
	p := filepath.Join(k.PackRoot, filepath.FromSlash(f.Source))
	if fileExists(p) {
		return p
	}
	return ""
}

func remasteredOffset(name string) (int64, bool) {
	base := strings.ToLower(filepath.Base(name))
	if !strings.HasPrefix(base, "-") {
		return 0, false
	}
	dot := strings.LastIndex(base, ".")
	if dot <= 1 {
		return 0, false
	}
	n, err := strconv.ParseInt(base[1:dot], 16, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func remasteredFilesByExt(dir, ext string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	ext = strings.ToLower(ext)
	out := []string{}
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ext {
			continue
		}
		out = append(out, e.Name())
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, aok := remasteredOffset(out[i])
		b, bok := remasteredOffset(out[j])
		if aok && bok && a != b {
			return a < b
		}
		if aok != bok {
			return aok
		}
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

// Custom pack model files were originally authored/tested against Kingdom Key's
// remastered WPN slots. The numeric filenames (for example -9a40.cvbl and
// -9b40.dds) are offsets inside that particular WPN and are NOT universal across
// all Keyblades. Resolve the requested Kingdom Key slot to the equivalent slot in
// the actual target Keyblade by matching its ordinal among files of the same type.
func resolveTargetRemasteredName(target *Keyblade, requested string) (string, error) {
	if target == nil || target.Stem == "" {
		return "", errors.New("target Keyblade has no WPN stem")
	}
	requested = filepath.ToSlash(filepath.Clean(requested))
	if requested == "." || requested == "" || strings.Contains(requested, "/") {
		return "", fmt.Errorf("unsupported remastered target path: %s", requested)
	}

	targetDir := filepath.Join(settings.OpenKhRoot, "remastered", "xw_ex_"+target.Stem+".wpn")
	if !dirExists(targetDir) {
		return "", fmt.Errorf("target remastered WPN folder not found: %s", targetDir)
	}

	ext := strings.ToLower(filepath.Ext(requested))
	if ext == "" {
		return "", fmt.Errorf("remastered target has no file extension: %s", requested)
	}

	// Determine which same-extension slot the requested Kingdom Key filename is.
	templateDir := filepath.Join(settings.OpenKhRoot, "remastered", "xw_ex_5010.wpn")
	templateFiles := remasteredFilesByExt(templateDir, ext)
	requestedBase := strings.ToLower(filepath.Base(requested))
	slot := -1
	for i, n := range templateFiles {
		if strings.ToLower(n) == requestedBase {
			slot = i
			break
		}
	}

	// The generator may be used with a partial remastered extraction. Keep known
	// Kingdom Key slot ordinals as a fallback so our current packs remain usable.
	if slot < 0 {
		switch requestedBase {
		case "-9a40.cvbl":
			slot = 0
		case "-4310.dds":
			slot = 0
		case "-9b40.dds":
			slot = 1
		case "-15f00.dds":
			slot = 2
		}
	}

	targetFiles := remasteredFilesByExt(targetDir, ext)

	// -9b40.dds is the main material texture paired with Kingdom Key's
	// -9a40.cvbl. Ordinal DDS positions are not stable across Keyblades, which
	// caused Soul Eater and other custom models to inherit the target Keyblade's
	// original texture. Map this slot by its relationship to the target CVBL:
	// choose the DDS whose offset is closest to CVBL+0x100 (the Kingdom Key
	// relationship), preferring numeric offsets.
	if requestedBase == "-9b40.dds" {
		cvblFiles := remasteredFilesByExt(targetDir, ".cvbl")
		if len(cvblFiles) > 0 {
			cvblName := cvblFiles[0]
			if cvOff, ok := remasteredOffset(cvblName); ok {
				expected := cvOff + 0x100
				best := ""
				bestDist := int64(^uint64(0) >> 1)
				for _, n := range targetFiles {
					if off, ok := remasteredOffset(n); ok {
						d := off - expected
						if d < 0 {
							d = -d
						}
						if d < bestDist {
							bestDist = d
							best = n
						}
					}
				}
				if best != "" {
					return best, nil
				}
			}
		}
	}

	// Kingdom Key D uses a second model texture (-15f00.dds) immediately after
	// Kingdom Key's main model texture in the DDS slot ordering. Once the main
	// texture has been resolved by CVBL proximity, map this secondary texture to
	// the next DDS slot after that resolved main slot. This avoids v1.1.7 cases
	// where ordinal mapping could collide with, or land on the wrong side of, the
	// target's main texture slot and scramble Kingdom Key D's colors.
	if requestedBase == "-15f00.dds" {
		mainName, mainErr := resolveTargetRemasteredName(target, "-9b40.dds")
		if mainErr == nil && mainName != "" {
			for i, n := range targetFiles {
				if strings.EqualFold(n, mainName) && i+1 < len(targetFiles) {
					return targetFiles[i+1], nil
				}
			}
		}
	}

	if slot >= 0 && slot < len(targetFiles) {
		return targetFiles[slot], nil
	}

	// Safe fallback for a type with exactly one target file (common for CVBL).
	if len(targetFiles) == 1 {
		return targetFiles[0], nil
	}

	return "", fmt.Errorf("could not map custom remastered slot %s to %s (found %d %s files)", requested, target.Name, len(targetFiles), ext)
}
func remasteredFilesReady(k *Keyblade) bool {
	if k == nil || len(k.RemasteredFiles) == 0 {
		return false
	}
	for _, f := range k.RemasteredFiles {
		if resolveRemasteredFile(k, f) == "" {
			return false
		}
	}
	return true
}
func sourceNeedsRaw(k *Keyblade) bool {
	if k == nil {
		return false
	}
	// Custom remastered-model packs normally preserve the target Keyblade's RAW WPN.
	// A RAW source is only required/copied when explicitly requested with copyRawWpn.
	if len(k.RemasteredFiles) > 0 && !k.CopyRawWpn {
		return false
	}
	return k.Builtin || k.RawWpn != "" || k.GameRawWpn != ""
}
func sourceModelReady(k *Keyblade) bool {
	if k == nil {
		return false
	}
	if len(k.RemasteredFiles) > 0 && !remasteredFilesReady(k) {
		return false
	}
	if sourceNeedsRaw(k) && resolveRawWpn(k) == "" {
		return false
	}
	return len(k.RemasteredFiles) > 0 || resolveRawWpn(k) != ""
}
func resolveSoundFolder(k *Keyblade) string {
	if k == nil {
		return ""
	}
	if k.AudioSourceID != "" {
		return resolveSoundFolder(getKeyblade(k.AudioSourceID))
	}
	if k.Builtin {
		p := filepath.Join(settings.OpenKhRoot, "remastered", "xw_ex_"+k.Stem+".se")
		if dirExists(p) {
			return p
		}
		return ""
	}
	if k.SoundFolder != "" {
		p := filepath.Join(k.PackRoot, k.SoundFolder)
		if dirExists(p) {
			return p
		}
	}
	if k.GameSoundFolder != "" {
		p := filepath.Join(settings.OpenKhRoot, "remastered", k.GameSoundFolder)
		if dirExists(p) {
			return p
		}
	}
	return ""
}
func soundNumber(name string) int {
	l := strings.ToLower(name)
	if !strings.HasPrefix(l, "se") || !strings.HasSuffix(l, ".win32.scd") {
		return math.MaxInt
	}
	mid := l[2 : len(l)-len(".win32.scd")]
	n, e := strconv.Atoi(mid)
	if e != nil {
		return math.MaxInt
	}
	return n
}
func uniqueSCDs(folder string) []string {
	if !dirExists(folder) {
		return nil
	}
	ents, _ := os.ReadDir(folder)
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.EqualFold(filepath.Ext(n), ".scd") && !sharedSCDs[strings.ToLower(n)] {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return soundNumber(out[i]) < soundNumber(out[j]) })
	return out
}

type SoundPair struct {
	Source string
	Target string
	Slot   int
}

type SoundMapInfo struct {
	Pairs        []SoundPair
	Mode         string
	TargetSlots  int
	MissingSlots []int
}

// buildSoundMap preserves semantic slot gaps when a smaller source bank spans
// the same numeric slot width as the target bank. This is required by KH1's
// Dream weapons: each has 27 unique SCDs spread across a 35-slot numeric range,
// with the same eight slot offsets absent from all three banks.
func buildSoundMap(src, dst []string) SoundMapInfo {
	info := SoundMapInfo{TargetSlots: len(dst)}
	if len(src) == 0 || len(dst) == 0 {
		return info
	}

	// Equal-size banks use the proven positional mapping used by vanilla Keyblades.
	if len(src) == len(dst) {
		info.Mode = "positional"
		for i := range src {
			info.Pairs = append(info.Pairs, SoundPair{Source: src[i], Target: dst[i], Slot: i})
		}
		return info
	}

	// Sparse indexed mapping: if the source IDs span exactly the same number of
	// slots as the contiguous target bank, preserve the source's numeric gaps.
	// Example: Dream Sword 11304..11338 spans 35 slots but only supplies 27.
	srcMin, srcMax := soundNumber(src[0]), soundNumber(src[len(src)-1])
	dstMin, dstMax := soundNumber(dst[0]), soundNumber(dst[len(dst)-1])
	srcSpan := srcMax - srcMin + 1
	dstSpan := dstMax - dstMin + 1
	if len(src) < len(dst) && srcMin != math.MaxInt && dstMin != math.MaxInt && srcSpan == len(dst) && dstSpan == len(dst) {
		used := make(map[int]bool)
		valid := true
		for _, n := range src {
			slot := soundNumber(n) - srcMin
			if slot < 0 || slot >= len(dst) || used[slot] {
				valid = false
				break
			}
			used[slot] = true
			info.Pairs = append(info.Pairs, SoundPair{Source: n, Target: dst[slot], Slot: slot})
		}
		if valid && len(info.Pairs) == len(src) {
			info.Mode = "sparse-indexed"
			for slot := 0; slot < len(dst); slot++ {
				if !used[slot] {
					info.MissingSlots = append(info.MissingSlots, slot)
				}
			}
			return info
		}
		info.Pairs = nil
	}

	// Unknown unequal banks keep the v1.1.1 conservative fallback so they remain
	// testable. This is intentionally distinct from a recognized sparse bank.
	info.Mode = "leading-partial"
	count := len(src)
	if len(dst) < count {
		count = len(dst)
	}
	for i := 0; i < count; i++ {
		info.Pairs = append(info.Pairs, SoundPair{Source: src[i], Target: dst[i], Slot: i})
	}
	for i := count; i < len(dst); i++ {
		info.MissingSlots = append(info.MissingSlots, i)
	}
	return info
}

func recordBase(k *Keyblade) int {
	if k == nil || !k.Builtin {
		return -1
	}
	return btlTableBase + k.RecordIndex*btlRecordSize
}
func gameplayRecordBase(k *Keyblade) int {
	if k == nil {
		return -1
	}
	idx := -1
	if k.Builtin {
		idx = k.RecordIndex
	} else if k.GameplayRecordIndex != nil {
		idx = *k.GameplayRecordIndex
	}
	if idx < 0 {
		return -1
	}
	return btlTableBase + idx*btlRecordSize
}
func getStats(k *Keyblade) (WeaponStats, error) {
	if k == nil {
		return WeaponStats{}, errors.New("missing Keyblade")
	}
	if base := gameplayRecordBase(k); base >= 0 {
		if base+btlRecordSize > len(baseBtl) {
			return WeaponStats{}, errors.New("btltbl.bin is not configured or does not match expected size")
		}
		mp := int(int8(baseBtl[base+offMP]))
		bits := binary.LittleEndian.Uint32(baseBtl[base+offReach : base+offReach+4])
		st := WeaponStats{Strength: int(baseBtl[base+offStrength]), MP: mp, CritRateRaw: int(baseBtl[base+offCritRate]), CritBonus: int(baseBtl[base+offCritBonus]), Recoil: int(baseBtl[base+offRecoil]), Reach: math.Float32frombits(bits)}
		// External packs may intentionally override only the source's default reach
		// while still borrowing the rest of their gameplay record from btltbl.bin.
		// This is used by Pack 1's Dream Sword and the custom-model packs.
		if !k.Builtin && k.Stats != nil && k.Stats.Reach != nil {
			st.Reach = *k.Stats.Reach
		}
		return st, nil
	}
	if k.Stats != nil && k.Stats.Reach != nil {
		return WeaponStats{Reach: *k.Stats.Reach}, nil
	}
	return WeaponStats{}, errors.New("no gameplay data available for custom source")
}
func setStats(b []byte, k *Keyblade, s WeaponStats, writeReach bool) error {
	base := recordBase(k)
	if base < 0 || base+btlRecordSize > len(b) {
		return fmt.Errorf("cannot modify stats for %s", k.Name)
	}
	if s.Strength < 0 || s.Strength > 255 || s.MP < -128 || s.MP > 127 || s.CritRateRaw < 0 || s.CritRateRaw > 255 || s.CritBonus < 0 || s.CritBonus > 255 || s.Recoil < 0 || s.Recoil > 255 {
		return errors.New("custom stat value is outside the supported byte range")
	}
	b[base+offStrength] = byte(s.Strength)
	b[base+offMP] = byte(int8(s.MP))
	b[base+offCritRate] = byte(s.CritRateRaw)
	b[base+offCritBonus] = byte(s.CritBonus)
	b[base+offRecoil] = byte(s.Recoil)
	if writeReach {
		binary.LittleEndian.PutUint32(b[base+offReach:base+offReach+4], math.Float32bits(s.Reach))
	}
	return nil
}
func setReach(b []byte, k *Keyblade, r float32) error {
	base := recordBase(k)
	if base < 0 || base+btlRecordSize > len(b) {
		return fmt.Errorf("cannot modify reach for %s", k.Name)
	}
	binary.LittleEndian.PutUint32(b[base+offReach:base+offReach+4], math.Float32bits(r))
	return nil
}
func critText(raw int) string {
	if raw == 200 {
		return "Always"
	}
	return fmt.Sprintf("x%g", float64(raw)/20.0)
}
func parseCrit(s string) (int, error) {
	t := strings.TrimSpace(s)
	if strings.EqualFold(t, "Always") {
		return 200, nil
	}
	t = strings.TrimPrefix(strings.TrimPrefix(t, "x"), "X")
	v, e := strconv.ParseFloat(t, 64)
	if e != nil {
		return 0, errors.New("Crit Rate must be like x0.5, x1, x2, or Always")
	}
	r := int(math.Round(v * 20))
	if r < 0 || r > 199 {
		return 0, errors.New("Crit Rate is outside supported range")
	}
	return r, nil
}

func selectedTarget() *Keyblade {
	if targetSel < 0 || targetSel >= len(targets) {
		return nil
	}
	return getKeyblade(targets[targetSel].ID)
}
func selectedSource() *Keyblade {
	if sourceSel < 0 || sourceSel >= len(sources) {
		return nil
	}
	return getKeyblade(sources[sourceSel].ID)
}

func updateSelectorButtons() {
	if len(targets) > 0 && targetSel >= 0 && targetSel < len(targets) {
		setText(targetButton, targets[targetSel].Name+"  ▼")
	} else {
		setText(targetButton, "Choose a Keyblade...  ▼")
	}
	if len(sources) > 0 && sourceSel >= 0 && sourceSel < len(sources) {
		setText(sourceButton, sources[sourceSel].Name+"  ▼")
	} else {
		setText(sourceButton, "Choose a Keyblade...  ▼")
	}
}

// showKeybladeMenu implements a dropdown-like selector with a Win32 popup menu.
// v1.0.1 used native COMBOBOX controls, which proved unstable on some Windows
// systems when the dropdown list was opened. A popup menu is simpler and avoids
// that control path entirely while keeping the same compact UI.
func showKeybladeMenu(anchor syscall.Handle, arr []Keyblade, current int) int {
	logEvent("showKeybladeMenu begin: anchor=0x%X count=%d current=%d", uintptr(anchor), len(arr), current)
	if len(arr) == 0 {
		logEvent("showKeybladeMenu aborted: empty array")
		return -1
	}
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return -1
	}
	defer procDestroyMenu.Call(menu)

	const baseID = 30000
	for i, k := range arr {
		label := k.Name
		if i == current {
			label = "✓  " + label
		}
		p, _ := syscall.UTF16PtrFromString(label)
		procAppendMenuW.Call(menu, MF_STRING, uintptr(baseID+i), uintptr(unsafe.Pointer(p)))
		runtime.KeepAlive(p)
	}

	var rc RECT
	ok, _, _ := procGetWindowRect.Call(uintptr(anchor), uintptr(unsafe.Pointer(&rc)))
	if ok == 0 {
		return -1
	}

	cmd, _, _ := procTrackPopupMenu.Call(
		menu,
		TPM_LEFTALIGN|TPM_TOPALIGN|TPM_RIGHTBUTTON|TPM_RETURNCMD|TPM_NONOTIFY,
		uintptr(rc.Left), uintptr(rc.Bottom),
		0, uintptr(mainWnd), 0,
	)
	if cmd < baseID || cmd >= baseID+uintptr(len(arr)) {
		logEvent("showKeybladeMenu cancelled/invalid cmd=%d", cmd)
		return -1
	}
	idx := int(cmd - baseID)
	logEvent("showKeybladeMenu selected: idx=%d name=%s", idx, arr[idx].Name)
	return idx
}

func reachClass(r float32) string {
	// Relative KH1 scale based on the spread of the 18 vanilla Sora Keyblades.
	// The underlying btltbl value is intentionally preserved for exact editing.
	switch {
	case r >= -3.40:
		return "Very Short"
	case r >= -3.60:
		return "Short"
	case r >= -4.15:
		return "Medium"
	case r >= -4.75:
		return "Long"
	default:
		return "Very Long"
	}
}

func formatReach(r float32) string { return fmt.Sprintf("%s (%.2f)", reachClass(r), r) }

func updateReachHint() {
	if lblReachHint == 0 || editReach == 0 {
		return
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(getText(editReach)), 32)
	if err != nil {
		setText(lblReachHint, "Relative KH1 length")
		return
	}
	setText(lblReachHint, reachClass(float32(v))+" length")
}

func updateInfo() {
	logEvent("updateInfo begin: targetSel=%d sourceSel=%d", targetSel, sourceSel)
	defer logEvent("updateInfo end")
	t := selectedTarget()
	s := selectedSource()
	if t != nil {
		if st, e := getStats(t); e == nil {
			setText(targetInfo, fmt.Sprintf("Strength: %d\r\nMP: %d\r\nCrit Rate: %s\r\nCrit Bonus: %d\r\nRecoil: %d\r\nReach: %s", st.Strength, st.MP, critText(st.CritRateRaw), st.CritBonus, st.Recoil, formatReach(st.Reach)))
			if !isChecked(chkCustom) {
				populateStatEdits(st)
			}
		} else {
			setText(targetInfo, "Gameplay data unavailable.\r\nConfigure OpenKH extraction data.")
		}
	}
	if s != nil {
		raw := "RAW model missing - prepare assets"
		if len(s.RemasteredFiles) > 0 {
			switch {
			case !remasteredFilesReady(s):
				raw = "custom model files missing"
			case sourceNeedsRaw(s) && resolveRawWpn(s) == "":
				raw = "base RAW missing - prepare assets"
			default:
				raw = "custom model ready"
			}
		} else if resolveRawWpn(s) != "" {
			raw = "RAW model ready"
		}
		audio := "audio missing"
		if len(s.RemasteredFiles) > 0 && s.SoundFolder == "" && s.GameSoundFolder == "" && s.AudioSourceID == "" {
			audio = "target audio"
		}
		srcSounds := uniqueSCDs(resolveSoundFolder(s))
		dstSounds := []string(nil)
		if t != nil {
			dstSounds = uniqueSCDs(resolveSoundFolder(t))
		}
		if len(srcSounds) > 0 {
			if len(dstSounds) > 0 {
				m := buildSoundMap(srcSounds, dstSounds)
				switch m.Mode {
				case "sparse-indexed":
					audio = fmt.Sprintf("audio ready (sparse %d/%d)", len(m.Pairs), len(dstSounds))
				case "leading-partial":
					audio = fmt.Sprintf("partial audio %d/%d", len(m.Pairs), len(dstSounds))
				default:
					audio = "audio ready"
				}
			} else {
				audio = "audio ready"
			}
		}
		setText(sourceStatus, fmt.Sprintf("%s | %s", raw, audio))
		if isChecked(chkMatch) {
			if st, e := getStats(s); e == nil {
				setText(editReach, fmt.Sprintf("%.2f", st.Reach))
			}
		}
	}
	updateStatEnable()
	updateReachHint()
}
func populateStatEdits(st WeaponStats) {
	setText(editStrength, strconv.Itoa(st.Strength))
	setText(editMP, strconv.Itoa(st.MP))
	setText(editCritRate, critText(st.CritRateRaw))
	setText(editCritBonus, strconv.Itoa(st.CritBonus))
	setText(editRecoil, strconv.Itoa(st.Recoil))
	setText(editReach, fmt.Sprintf("%.2f", st.Reach))
	updateReachHint()
}
func updateStatEnable() {
	custom := isChecked(chkCustom)
	enable(editStrength, custom)
	enable(editMP, custom)
	enable(editCritRate, custom)
	enable(editCritBonus, custom)
	enable(editRecoil, custom)
	enable(editReach, custom && !isChecked(chkMatch))
}

func parseIntEdit(h syscall.Handle, name string, min, max int) (int, error) {
	v, e := strconv.Atoi(strings.TrimSpace(getText(h)))
	if e != nil || v < min || v > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return v, nil
}
func readCustomStats() (WeaponStats, error) {
	str, e := parseIntEdit(editStrength, "Strength", 0, 255)
	if e != nil {
		return WeaponStats{}, e
	}
	mp, e := parseIntEdit(editMP, "MP", -128, 127)
	if e != nil {
		return WeaponStats{}, e
	}
	cr, e := parseCrit(getText(editCritRate))
	if e != nil {
		return WeaponStats{}, e
	}
	cb, e := parseIntEdit(editCritBonus, "Crit Bonus", 0, 255)
	if e != nil {
		return WeaponStats{}, e
	}
	rc, e := parseIntEdit(editRecoil, "Recoil", 0, 255)
	if e != nil {
		return WeaponStats{}, e
	}
	rv, e := strconv.ParseFloat(strings.TrimSpace(getText(editReach)), 32)
	if e != nil {
		return WeaponStats{}, errors.New("Reach must be a number")
	}
	return WeaponStats{Strength: str, MP: mp, CritRateRaw: cr, CritBonus: cb, Recoil: rc, Reach: float32(rv)}, nil
}

func queueDisplayLines(q QueueItem) []string {
	t := getKeyblade(q.TargetID)
	s := getKeyblade(q.SourceID)
	tn, sn := q.TargetID, q.SourceID
	if t != nil {
		tn = t.Name
	}
	if s != nil {
		sn = s.Name
	}
	lines := []string{sn + "  ->  " + tn}
	extras := []string{}
	if q.MatchLength {
		extras = append(extras, "match length")
	}
	if q.CustomStats {
		extras = append(extras, "custom stats")
	}
	if len(extras) > 0 {
		// Standard Win32 LISTBOX controls do not word-wrap. A second indented
		// row gives us predictable wrapping while keeping each replacement easy
		// to scan in narrow windows.
		lines = append(lines, "    ["+strings.Join(extras, ", ")+"]")
	}
	return lines
}

func addListString(text string) {
	p, _ := syscall.UTF16PtrFromString(text)
	send(listQueue, LB_ADDSTRING, 0, uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
}

func refreshQueueList() {
	send(listQueue, LB_RESETCONTENT, 0, 0)
	queueRowMap = nil
	if btnAdd != 0 {
		setText(btnAdd, "Add / Update Replacement")
	}
	for qi, q := range queue {
		for _, line := range queueDisplayLines(q) {
			addListString(line)
			queueRowMap = append(queueRowMap, qi)
		}
	}
}

func selectedQueueIndex() int {
	row := int(send(listQueue, LB_GETCURSEL, 0, 0))
	if row < 0 || row >= len(queueRowMap) {
		return -1
	}
	return queueRowMap[row]
}

func addOrUpdateQueue() {
	t, s := selectedTarget(), selectedSource()
	if t == nil || s == nil {
		showError("Choose both a target and source Keyblade.")
		return
	}
	if !sourceModelReady(s) {
		if s != nil && sourceNeedsRaw(s) && resolveRawWpn(s) == "" && (s.Builtin || s.GameRawWpn != "") && askYesNo("The base RAW model for "+s.Name+" has not been prepared yet.\r\n\r\nPrepare the required game assets now?", "Prepare Assets") {
			prepareRaw()
			s = selectedSource()
		}
		if s == nil || !sourceModelReady(s) {
			name := "the selected source"
			if s != nil {
				name = s.Name
			}
			showError("Source model assets are still missing for " + name + ".\r\n\r\nUse Prepare Assets if the source needs a game RAW file, or check that the external pack's asset folder was copied into Packs along with its JSON file.")
			return
		}
	}
	q := QueueItem{TargetID: t.ID, SourceID: s.ID, MatchLength: isChecked(chkMatch), CustomStats: isChecked(chkCustom)}
	if q.CustomStats {
		st, e := readCustomStats()
		if e != nil {
			showError(e.Error())
			return
		}
		q.Stats = st
	} else {
		st, e := getStats(t)
		if e == nil {
			q.Stats = st
		}
	}
	found := -1
	for i := range queue {
		if queue[i].TargetID == q.TargetID {
			found = i
			break
		}
	}
	if found >= 0 {
		queue[found] = q
	} else {
		queue = append(queue, q)
	}
	refreshQueueList()
}
func removeSelected() {
	i := selectedQueueIndex()
	if i >= 0 && i < len(queue) {
		queue = append(queue[:i], queue[i+1:]...)
		refreshQueueList()
	}
}
func clearQueue() { queue = nil; refreshQueueList() }
func deselectQueue() {
	// -1 clears the selection in a single-select Win32 LISTBOX.
	send(listQueue, LB_SETCURSEL, ^uintptr(0), 0)
	setText(btnAdd, "Add / Update Replacement")
	logEvent("Replacement list deselected")
}

func sanitizeFileName(s string) string {
	bad := `<>:"/\\|?*`
	for _, r := range bad {
		s = strings.ReplaceAll(s, string(r), "_")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		s = "Generated Keyblade Replacer"
	}
	return s
}
func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func replacementDescriptionLine(q QueueItem) string {
	t := getKeyblade(q.TargetID)
	s := getKeyblade(q.SourceID)
	tn, sn := q.TargetID, q.SourceID
	if t != nil {
		tn = t.Name
	}
	if s != nil {
		sn = s.Name
	}
	extras := []string{}
	if q.MatchLength {
		extras = append(extras, "match length")
	}
	if q.CustomStats {
		extras = append(extras, "custom stats")
	}
	line := sn + " -> " + tn
	if len(extras) > 0 {
		line += " [" + strings.Join(extras, ", ") + "]"
	}
	return line
}
func writeModDescription(y *strings.Builder) {
	y.WriteString("description: |-\r\n")
	y.WriteString("  Replaces the following Keyblades:\r\n")
	y.WriteString("  \r\n")
	for _, q := range queue {
		y.WriteString("  " + replacementDescriptionLine(q) + "\r\n")
	}
}
func copyFile(src, dst string) error {
	if e := os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
		return e
	}
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.Create(dst)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	ce := out.Close()
	if e != nil {
		return e
	}
	return ce
}

func generateMod(parent, modName, author string) (string, error) {
	if len(queue) == 0 {
		return "", errors.New("add at least one replacement first")
	}
	if !fileExists(filepath.Join(settings.OpenKhRoot, "btltbl.bin")) || !dirExists(filepath.Join(settings.OpenKhRoot, "remastered")) {
		return "", errors.New("OpenKH extraction data is not configured")
	}
	refreshBaseData()
	safe := sanitizeFileName(modName)
	zipPath := filepath.Join(parent, safe+".zip")
	if fileExists(zipPath) {
		if !askYesNo("The output ZIP already exists:\r\n"+zipPath+"\r\n\r\nReplace it?", "Generate Mod") {
			return "", errors.New("cancelled")
		}
	}
	_ = os.Remove(zipPath)
	outDir, e := os.MkdirTemp("", "KH1KeybladeGeneratorBuild-*")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(outDir)
	if e := os.MkdirAll(filepath.Join(outDir, "files"), 0755); e != nil {
		return "", e
	}
	setBusyStatus("Building mod files...")
	var y strings.Builder
	fmt.Fprintf(&y, "title: %s\r\n", yamlQuote(modName))
	fmt.Fprintf(&y, "originalAuthor: %s\r\n", yamlQuote(author))
	writeModDescription(&y)
	y.WriteString("game: kh1\r\n\r\nassets:\r\n")
	copiedRaw := map[string]string{}
	copiedSounds := map[string]string{}
	needsBtl := false
	btl := append([]byte(nil), baseBtl...)
	for _, item := range queue {
		target, source := getKeyblade(item.TargetID), getKeyblade(item.SourceID)
		if target == nil || source == nil {
			return "", errors.New("queued Keyblade definition is no longer available")
		}
		setBusyStatus("Adding " + source.Name + " -> " + target.Name + "...")
		raw := resolveRawWpn(source)
		if !sourceModelReady(source) {
			return "", fmt.Errorf("source model assets missing for %s", source.Name)
		}
		// Remastered-model sources preserve the target's existing RAW WPN by default.
		// This is critical for NPC/alternate weapon packs: copying Kingdom Key RAW here
		// would turn every target into Kingdom Key before the remastered override is applied.
		if raw != "" && (len(source.RemasteredFiles) == 0 || source.CopyRawWpn) {
			rawName := ""
			if source.Builtin {
				rawName = "xw_ex_" + source.Stem + ".wpn"
			} else {
				rawName = sanitizeFileName(source.ID) + ".wpn"
			}
			rawRel := "files/raw/" + rawName
			if _, ok := copiedRaw[source.ID]; !ok {
				if e := copyFile(raw, filepath.Join(outDir, filepath.FromSlash(rawRel))); e != nil {
					return "", e
				}
				copiedRaw[source.ID] = rawRel
			}
			fmt.Fprintf(&y, "  # %s -> %s\r\n  - method: copy\r\n    name: raw/xw_ex_%s.wpn\r\n    package: kh1_first\r\n    source:\r\n      - name: %s\r\n\r\n", source.Name, target.Name, target.Stem, copiedRaw[source.ID])
		}
		if len(source.RemasteredFiles) > 0 {
			for fileIndex, rf := range source.RemasteredFiles {
				src := resolveRemasteredFile(source, rf)
				if src == "" {
					return "", fmt.Errorf("custom remastered model file missing for %s: %s", source.Name, rf.Source)
				}
				targetRel, mapErr := resolveTargetRemasteredName(target, rf.Target)
				if mapErr != nil {
					return "", fmt.Errorf("could not map %s custom model onto %s: %v", source.Name, target.Name, mapErr)
				}
				logEvent("custom remastered slot mapping for %s -> %s: %s => %s", source.Name, target.Name, rf.Target, targetRel)
				copyName := fmt.Sprintf("%02d_%s", fileIndex, filepath.Base(targetRel))
				assetRel := filepath.ToSlash(filepath.Join("files", "remastered_sources", sanitizeFileName(source.ID), copyName))
				if e := copyFile(src, filepath.Join(outDir, filepath.FromSlash(assetRel))); e != nil {
					return "", e
				}
				fmt.Fprintf(&y, "  # %s custom remastered model -> %s\r\n  - method: copy\r\n    name: remastered/xw_ex_%s.wpn/%s\r\n    source:\r\n      - name: %s\r\n\r\n", source.Name, target.Name, target.Stem, targetRel, assetRel)
			}
		}
		sf, tf := resolveSoundFolder(source), resolveSoundFolder(target)
		if sf != "" && tf != "" {
			src, dst := uniqueSCDs(sf), uniqueSCDs(tf)
			if len(src) > 0 && len(dst) > 0 {
				mapping := buildSoundMap(src, dst)
				switch mapping.Mode {
				case "sparse-indexed":
					logEvent("sparse indexed sound mapping for %s -> %s: source=%d target=%d mapped=%d missingSlots=%v; numeric source gaps preserved", source.Name, target.Name, len(src), len(dst), len(mapping.Pairs), mapping.MissingSlots)
					setBusyStatus(fmt.Sprintf("Adding %s -> %s... sparse audio %d/%d", source.Name, target.Name, len(mapping.Pairs), len(dst)))
				case "leading-partial":
					logEvent("partial leading sound bank mapping for %s -> %s: source=%d target=%d mapped=%d; unmatched target sounds remain unchanged", source.Name, target.Name, len(src), len(dst), len(mapping.Pairs))
					setBusyStatus(fmt.Sprintf("Adding %s -> %s... partial audio %d/%d", source.Name, target.Name, len(mapping.Pairs), len(dst)))
				}
				sub := sanitizeFileName(source.ID)
				if _, ok := copiedSounds[source.ID]; !ok {
					for _, n := range src {
						if e := copyFile(filepath.Join(sf, n), filepath.Join(outDir, "files", "sounds", sub, n)); e != nil {
							return "", e
						}
					}
					copiedSounds[source.ID] = sub
				}
				for _, pair := range mapping.Pairs {
					fmt.Fprintf(&y, "  - method: copy\r\n    name: remastered/xw_ex_%s.se/%s\r\n    source:\r\n      - name: files/sounds/%s/%s\r\n\r\n", target.Stem, pair.Target, sub, pair.Source)
				}
			}
		}
		if item.CustomStats {
			if len(btl) == 0 {
				return "", errors.New("btltbl.bin is required for custom gameplay stats")
			}
			if e := setStats(btl, target, item.Stats, !item.MatchLength); e != nil {
				return "", e
			}
			needsBtl = true
		}
		if item.MatchLength {
			if len(btl) == 0 {
				return "", errors.New("btltbl.bin is required to match Keyblade lengths")
			}
			ss, e := getStats(source)
			if e != nil {
				return "", fmt.Errorf("no reach value for source %s: %v", source.Name, e)
			}
			if e = setReach(btl, target, ss.Reach); e != nil {
				return "", e
			}
			needsBtl = true
		} else if !item.CustomStats && !source.Builtin && source.Stats != nil && source.Stats.Reach != nil {
			// Pack-defined reaches are the source's default gameplay length. Apply
			// them even when Match Keyblade Lengths / Custom Stats are left off.
			if len(btl) == 0 {
				return "", errors.New("btltbl.bin is required to apply the source's default Keyblade length")
			}
			if e := setReach(btl, target, *source.Stats.Reach); e != nil {
				return "", e
			}
			logEvent("default external reach for %s -> %s: %.2f", source.Name, target.Name, *source.Stats.Reach)
			needsBtl = true
		}
	}
	if needsBtl {
		setBusyStatus("Writing combined gameplay-data edits...")
		p := filepath.Join(outDir, "files", "btltbl.bin")
		if e := os.WriteFile(p, btl, 0644); e != nil {
			return "", e
		}
		y.WriteString("  # Combined gameplay-data edits\r\n  - method: copy\r\n    name: btltbl.bin\r\n    package: kh1_first\r\n    source:\r\n      - name: files/btltbl.bin\r\n\r\n")
	}
	if e := os.WriteFile(filepath.Join(outDir, "mod.yml"), []byte(y.String()), 0644); e != nil {
		return "", e
	}
	man := Manifest{GeneratorVersion: appVersion, ModName: modName, Author: author, GeneratedAt: time.Now().Format(time.RFC3339), Replacements: queue}
	mb, _ := json.MarshalIndent(man, "", "  ")
	_ = os.WriteFile(filepath.Join(outDir, "generator_manifest.json"), mb, 0644)
	setBusyStatus("Creating OpenKH ZIP...")
	if e := zipDirectory(outDir, zipPath); e != nil {
		_ = os.Remove(zipPath)
		return "", e
	}
	return zipPath, nil
}
func zipDirectory(src, dst string) error {
	f, e := os.Create(dst)
	if e != nil {
		return e
	}
	zw := zip.NewWriter(f)
	e = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		h, e := zip.FileInfoHeader(info)
		if e != nil {
			return e
		}
		h.Name = filepath.ToSlash(rel)
		h.Method = zip.Deflate
		w, e := zw.CreateHeader(h)
		if e != nil {
			return e
		}
		in, e := os.Open(path)
		if e != nil {
			return e
		}
		_, e = io.Copy(w, in)
		in.Close()
		return e
	})
	ce := zw.Close()
	fe := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return fe
}

func hresultFailed(hr uintptr) bool { return int32(hr) < 0 }

func comMethod(obj uintptr, index uintptr) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(obj))
	return *(*uintptr)(unsafe.Pointer(vtbl + index*unsafe.Sizeof(uintptr(0))))
}

func comCall(obj uintptr, index uintptr, args ...uintptr) uintptr {
	fn := comMethod(obj, index)
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, obj)
	all = append(all, args...)
	r1, _, _ := syscall.SyscallN(fn, all...)
	return r1
}

func comRelease(obj uintptr) {
	if obj != 0 {
		comCall(obj, 2)
	}
}

func shellItemForPath(path string) uintptr {
	if !dirExists(path) {
		return 0
	}
	p, _ := syscall.UTF16PtrFromString(path)
	var item uintptr
	hr, _, _ := procSHCreateItemFromParsingName.Call(
		uintptr(unsafe.Pointer(p)),
		0,
		uintptr(unsafe.Pointer(&iidIShellItem)),
		uintptr(unsafe.Pointer(&item)),
	)
	runtime.KeepAlive(p)
	if hresultFailed(hr) {
		return 0
	}
	return item
}

func utf16PtrString(p uintptr) string {
	if p == 0 {
		return ""
	}
	buf := (*[1 << 20]uint16)(unsafe.Pointer(p))[:]
	for i, c := range buf {
		if c == 0 {
			return syscall.UTF16ToString(buf[:i])
		}
	}
	return ""
}

// browseFolderExplorer uses the modern Windows Common Item Dialog. Unlike the
// legacy SHBrowseForFolder tree, this is the normal File Explorer-style folder
// picker with the address bar, navigation pane, resizing, and familiar layout.
func browseFolderExplorer(owner syscall.Handle, title, initial string) (string, bool, error) {
	var dlg uintptr
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)),
		0,
		CLSCTX_INPROC_SERVER,
		uintptr(unsafe.Pointer(&iidIFileOpenDialog)),
		uintptr(unsafe.Pointer(&dlg)),
	)
	if hresultFailed(hr) || dlg == 0 {
		return "", false, fmt.Errorf("File Explorer folder picker unavailable (HRESULT 0x%08X)", uint32(hr))
	}
	defer comRelease(dlg)

	var options uint32
	if hr = comCall(dlg, 10, uintptr(unsafe.Pointer(&options))); !hresultFailed(hr) { // IFileDialog::GetOptions
		options |= FOS_PICKFOLDERS | FOS_FORCEFILESYSTEM | FOS_PATHMUSTEXIST | FOS_NOCHANGEDIR
		if hr = comCall(dlg, 9, uintptr(options)); hresultFailed(hr) { // SetOptions
			return "", false, fmt.Errorf("could not configure folder picker (HRESULT 0x%08X)", uint32(hr))
		}
	}

	if title != "" {
		t, _ := syscall.UTF16PtrFromString(title)
		_ = comCall(dlg, 17, uintptr(unsafe.Pointer(t))) // IFileDialog::SetTitle
		runtime.KeepAlive(t)
	}
	if initial != "" {
		if item := shellItemForPath(initial); item != 0 {
			_ = comCall(dlg, 12, item) // IFileDialog::SetFolder
			comRelease(item)
		}
	}

	hr = comCall(dlg, 3, uintptr(owner)) // IModalWindow::Show
	if uint32(hr) == 0x800704C7 {        // HRESULT_FROM_WIN32(ERROR_CANCELLED)
		return "", false, nil
	}
	if hresultFailed(hr) {
		return "", false, fmt.Errorf("folder picker failed (HRESULT 0x%08X)", uint32(hr))
	}

	var item uintptr
	hr = comCall(dlg, 20, uintptr(unsafe.Pointer(&item))) // IFileDialog::GetResult
	if hresultFailed(hr) || item == 0 {
		return "", false, fmt.Errorf("could not read selected folder (HRESULT 0x%08X)", uint32(hr))
	}
	defer comRelease(item)

	var pathPtr uintptr
	hr = comCall(item, 5, SIGDN_FILESYSPATH, uintptr(unsafe.Pointer(&pathPtr))) // IShellItem::GetDisplayName
	if hresultFailed(hr) || pathPtr == 0 {
		return "", false, fmt.Errorf("could not resolve selected folder path (HRESULT 0x%08X)", uint32(hr))
	}
	defer procCoTaskMemFree.Call(pathPtr)
	path := utf16PtrString(pathPtr)
	if path == "" {
		return "", false, errors.New("selected folder did not have a filesystem path")
	}
	return path, true, nil
}

func browseFolderLegacy(owner syscall.Handle, title string) (string, bool) {
	display := make([]uint16, 260)
	bi := BROWSEINFO{HwndOwner: owner, PszDisplayName: &display[0], LpszTitle: utf16(title), UlFlags: BIF_RETURNONLYFSDIRS | BIF_NEWDIALOGSTYLE}
	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "", false
	}
	defer procCoTaskMemFree.Call(pidl)
	path := make([]uint16, 32768)
	ok, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	if ok == 0 {
		return "", false
	}
	return syscall.UTF16ToString(path), true
}

func browseFolder(owner syscall.Handle, title, initial string) (string, bool) {
	if p, ok, err := browseFolderExplorer(owner, title, initial); err == nil {
		return p, ok
	} else {
		logEvent("Modern folder picker failed; falling back to legacy picker: %v", err)
	}
	return browseFolderLegacy(owner, title)
}

func browseExe(owner syscall.Handle, title, initial string) (string, bool) {
	buf := make([]uint16, 32768)
	filter := make([]uint16, 0, 80)
	for _, part := range []string{"Executable files (*.exe)", "*.exe", "All files (*.*)", "*.*"} {
		u, _ := syscall.UTF16FromString(part)
		filter = append(filter, u...)
	}
	filter = append(filter, 0) // final extra NUL required by OPENFILENAME filter strings
	var initialPtr *uint16
	if initial != "" {
		if fileExists(initial) {
			initial = filepath.Dir(initial)
		}
		if dirExists(initial) {
			initialPtr, _ = syscall.UTF16PtrFromString(initial)
		}
	}
	ofn := OPENFILENAME{LStructSize: uint32(unsafe.Sizeof(OPENFILENAME{})), HwndOwner: owner, LpstrFilter: &filter[0], NFilterIndex: 1, LpstrFile: &buf[0], NMaxFile: uint32(len(buf)), LpstrInitialDir: initialPtr, LpstrTitle: utf16(title), Flags: OFN_FILEMUSTEXIST | OFN_PATHMUSTEXIST | OFN_EXPLORER}
	r, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	runtime.KeepAlive(initialPtr)
	if r == 0 {
		return "", false
	}
	return syscall.UTF16ToString(buf), true
}

func runSetup() {
	showInfo("Choose your OpenKH KH1 extraction folder first.\r\n\r\nIt should contain btltbl.bin and the remastered folder.")
	if p, ok := browseFolder(mainWnd, "Choose C:\\OpenKH Extractions\\kh1 (or your equivalent)", settings.OpenKhRoot); ok {
		settings.OpenKhRoot = p
	}
	showInfo("Now choose the game's Image\\dt folder.\r\n\r\nThis is used to find RAW package extractions.")
	if p, ok := browseFolder(mainWnd, "Choose KINGDOM HEARTS ...\\Image\\dt", settings.GameDt); ok {
		settings.GameDt = p
	}
	if askYesNo("Would you like to select KHPCPatchManager.exe now?\r\n\r\nThis is used to prepare/cache missing game-provided RAW source assets.", "Data Setup") {
		if p, ok := browseExe(mainWnd, "Choose KHPCPatchManager.exe", settings.PatchManagerExe); ok {
			settings.PatchManagerExe = p
		}
	}
	saveSettings()
	refreshBaseData()
	updateInfo()
}
func packGameRawTotals() (ready, total int) {
	seen := map[string]bool{}
	for i := range keyblades {
		k := &keyblades[i]
		if k.Builtin || k.GameRawWpn == "" {
			continue
		}
		name := strings.ToLower(filepath.Base(k.GameRawWpn))
		if seen[name] {
			continue
		}
		seen[name] = true
		total++
		if resolveRawWpn(k) != "" {
			ready++
		}
	}
	return
}
func packRemasteredTotals() (ready, total int) {
	for i := range keyblades {
		k := &keyblades[i]
		if k.Builtin || len(k.RemasteredFiles) == 0 {
			continue
		}
		total++
		if remasteredFilesReady(k) {
			ready++
		}
	}
	return
}
func allRequiredRawReady() bool {
	pr, pt := packGameRawTotals()
	mr, mt := packRemasteredTotals()
	return countBuiltinRaw() >= 18 && pr >= pt && mr >= mt
}
func rawReadySummary() string {
	pr, pt := packGameRawTotals()
	mr, mt := packRemasteredTotals()
	parts := []string{"18/18 vanilla Keyblades"}
	if pt > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d external-pack game assets", pr, pt))
	}
	if mt > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d custom pack models", mr, mt))
	}
	return strings.Join(parts, " and ")
}

func prepareRaw() {
	if !dirExists(settings.GameDt) {
		showError("Game Image\\dt folder is not configured. Click Data Setup first.")
		return
	}
	setBusyStatus("Scanning existing RAW weapon assets...")
	defer clearBusyStatus()

	// First cache anything the user has already extracted. This can complete
	// setup without running KHPCPatchManager at all.
	if _, err := cacheExistingRawAssets(); err != nil {
		showError("Could not update the RAW asset cache:\r\n" + err.Error())
		return
	} else if allRequiredRawReady() {
		updateInfo()
		showInfo("All required RAW source models are ready (" + rawReadySummary() + ").\r\n\r\nCache:\r\n" + assetCacheDir())
		return
	}

	if !fileExists(settings.PatchManagerExe) {
		if p, ok := browseExe(mainWnd, "Choose KHPCPatchManager.exe", settings.PatchManagerExe); ok {
			settings.PatchManagerExe = p
			saveSettings()
		} else {
			return
		}
	}

	builtinMissing := 18 - countBuiltinRaw()
	pr, pt := packGameRawTotals()
	packMissing := pt - pr
	parts := []string{}
	if builtinMissing > 0 {
		parts = append(parts, fmt.Sprintf("%d vanilla Keyblade model(s)", builtinMissing))
	}
	if packMissing > 0 {
		parts = append(parts, fmt.Sprintf("%d external-pack game model(s)", packMissing))
	}
	if !askYesNo("The generator still needs "+strings.Join(parts, " and ")+".\r\n\r\nIt will use KHPCPatchManager to RAW-extract KH1 package data, copy only the required weapon .wpn files into its compact local cache, and remove any temporary .hed_out folders that it created itself.\r\n\r\nExisting .hed_out folders are never deleted.\r\n\r\nContinue?", "Prepare Assets") {
		return
	}

	// Most/all Sora weapon assets are expected in kh1_first, so try it first and
	// stop as soon as all vanilla + external-pack game-backed source models are found.
	heds := []string{"kh1_first.hed", "kh1_second.hed", "kh1_third.hed", "kh1_fourth.hed", "kh1_fifth.hed"}
	attempted := 0
	for _, n := range heds {
		if allRequiredRawReady() {
			break
		}
		p := filepath.Join(settings.GameDt, n)
		if !fileExists(p) {
			continue
		}
		outDir := p + "_out"
		preExisting := dirExists(outDir)
		if preExisting {
			setBusyStatus("Caching weapon models from " + filepath.Base(outDir) + "...")
			logEvent("Using pre-existing RAW extraction: %s", outDir)
			if _, err := cacheExistingRawAssets(); err != nil {
				showError("Could not cache assets from " + filepath.Base(outDir) + ":\r\n" + err.Error())
				return
			}
			continue
		}

		attempted++
		setText(lblConfig, fmt.Sprintf("Preparing game assets: extracting %s...", filepath.Base(p)))
		setBusyStatus("Extracting " + filepath.Base(p) + " with KHPCPatchManager...")
		logEvent("RAW extraction begin: %s", p)
		cmd := exec.Command(settings.PatchManagerExe, p, "-raw")
		cmd.Dir = filepath.Dir(settings.PatchManagerExe)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if e := cmd.Run(); e != nil {
			logEvent("RAW extraction failed: %s | %v", p, e)
			showError("KHPCPatchManager failed while extracting " + filepath.Base(p) + ":\r\n" + e.Error())
			refreshBaseData()
			return
		}
		logEvent("RAW extraction finished: %s", p)
		setBusyStatus("Caching extracted weapon models...")
		if _, err := cacheExistingRawAssets(); err != nil {
			showError("Extraction finished, but the generator could not cache the weapon models:\r\n" + err.Error())
			return
		}
		// Only delete output that did not exist before this function ran.
		if dirExists(outDir) {
			if err := os.RemoveAll(outDir); err != nil {
				logEvent("Could not remove temporary extraction %s: %v", outDir, err)
			}
		}
		refreshBaseData()
	}

	refreshBaseData()
	updateInfo()
	if allRequiredRawReady() {
		showInfo("Asset preparation finished.\r\n\r\nReady: " + rawReadySummary() + ".\r\n\r\nCache:\r\n" + assetCacheDir())
		return
	}
	br := countBuiltinRaw()
	pr, pt = packGameRawTotals()
	mr, mt := packRemasteredTotals()
	detail := fmt.Sprintf("Vanilla RAW models: %d/18", br)
	if pt > 0 {
		detail += fmt.Sprintf("\r\nExternal-pack game assets: %d/%d", pr, pt)
	}
	if mt > 0 {
		detail += fmt.Sprintf("\r\nCustom pack model files: %d/%d", mr, mt)
	}
	if attempted == 0 {
		showError("Some required RAW models are still missing.\r\n\r\n" + detail + "\r\n\r\nNo additional KH1 .hed packages were available to extract. Check Data Setup and your KH1 Image\\dt folder.")
	} else {
		showError("RAW preparation completed, but some required weapon models were not found.\r\n\r\n" + detail + "\r\n\r\nPlease send the current diagnostic log so we can see which package/output path differs on this installation.")
	}
}
func updateConfigLabel() {
	if lblConfig == 0 {
		return
	}
	btl := fileExists(filepath.Join(settings.OpenKhRoot, "btltbl.bin"))
	rem := dirExists(filepath.Join(settings.OpenKhRoot, "remastered"))
	pr, pt := packGameRawTotals()
	mr, mt := packRemasteredTotals()
	text := fmt.Sprintf("Data: btltbl %s | remastered %s | RAW models %d/18", yesNo(btl), yesNo(rem), countBuiltinRaw())
	if pt > 0 {
		text += fmt.Sprintf(" | Pack RAW %d/%d", pr, pt)
	}
	if mt > 0 {
		text += fmt.Sprintf(" | Pack models %d/%d", mr, mt)
	}
	setText(lblConfig, text)
}
func yesNo(v bool) string {
	if v {
		return "ready"
	}
	return "missing"
}
func countBuiltinRaw() int {
	n := 0
	for i := range targets {
		if resolveRawWpn(&targets[i]) != "" {
			n++
		}
	}
	return n
}

func generateClicked() {
	if len(queue) == 0 {
		showError("Add at least one replacement first.")
		return
	}
	parent := buildOutputPath()
	if err := os.MkdirAll(parent, 0755); err != nil {
		showError("Could not create the configured save path:\r\n" + parent + "\r\n\r\n" + err.Error() + "\r\n\r\nUse Change Save Path... to choose another folder.")
		return
	}
	name := strings.TrimSpace(getText(editModName))
	if name == "" {
		name = "Generated Keyblade Replacer"
	}
	author := strings.TrimSpace(getText(editAuthor))
	setBusyStatus("Starting mod generation...")
	zip, e := generateMod(parent, name, author)
	clearBusyStatus()
	if e != nil {
		if e.Error() != "cancelled" {
			showError(e.Error())
		}
		return
	}
	showInfo("Generated successfully.\r\n\r\nOpenKH-ready ZIP:\r\n" + zip)
}

func openDebugLog() {
	p := sessionLogPath
	if lastCrashPath != "" && fileExists(lastCrashPath) {
		p = lastCrashPath
	}
	if p == "" || !fileExists(p) {
		showInfo("No debug log is available yet.")
		return
	}
	logEvent("Opening debug report: %s", p)
	cmd := exec.Command("notepad.exe", p)
	if err := cmd.Start(); err != nil {
		showError("Could not open the debug report.\r\n\r\n" + p + "\r\n\r\n" + err.Error())
	}
}

func createUI() {
	// Header
	addLabel("KH1 Keyblade Generator", 20, 18, 330, 34)
	addLabel("Build one OpenKH mod from any combination of KH1 Keyblade swaps.", 20, 50, 620, 26)
	lblConfig = addLabel("", 660, 24, 520, 26)
	btnSetup = addButton(idSetup, "Data Setup...", 714, 52, 120, 30)
	btnPrepare = addButton(idPrepare, "Prepare Assets...", 842, 52, 145, 30)
	btnOpenLog = addButton(idOpenLog, "Open Last Crash Report", 995, 52, 185, 30)

	// columns
	addLabel("Replace:", 22, 105, 120, 28)
	targetButton = addButton(idTargetButton, "Choose a Keyblade...  ▼", 22, 132, 330, 32)
	addLabel("Current target gameplay data", 22, 175, 300, 28)
	targetInfo = addLabel("", 22, 205, 330, 150)
	lblStatHelp = createControl(0, "STATIC", "", WS_CHILD|WS_BORDER|SS_LEFT, 22, 370, 350, 310, mainWnd, 0)

	addLabel("With:", 390, 105, 120, 28)
	sourceButton = addButton(idSourceButton, "Choose a Keyblade...  ▼", 390, 132, 330, 32)
	sourceStatus = addLabel("", 390, 168, 330, 28)

	addLabel("Gameplay data", 390, 205, 220, 28)
	labels := []struct {
		n string
		y int32
	}{{"Strength:", 238}, {"MP:", 273}, {"Crit Rate:", 308}, {"Crit Bonus:", 343}, {"Recoil:", 378}, {"Reach:", 413}}
	statLabelHandles := make([]syscall.Handle, 0, len(labels))
	for _, d := range labels {
		statLabelHandles = append(statLabelHandles, addLabel(d.n, 410, d.y, 110, 28))
	}
	editStrength = addEdit(idStrength, 520, 235, 165, 31)
	editMP = addEdit(idMP, 520, 270, 165, 31)
	editCritRate = addEdit(idCritRate, 520, 305, 165, 31)
	editCritBonus = addEdit(idCritBonus, 520, 340, 165, 31)
	editRecoil = addEdit(idRecoil, 520, 375, 165, 31)
	editReach = addEdit(idReach, 520, 410, 165, 31)
	lblReachHint = addLabel("Relative KH1 length", 520, 443, 190, 28)
	btnAdd = addButton(idAdd, "Add / Update Replacement", 447, 468, 215, 34)

	registerStatHelp("STRENGTH\r\nAdds to Sora's Strength while this Keyblade is equipped. Strength controls damage from weapon attacks and Strength-based abilities; higher values generally mean more physical damage.\r\n\r\nGenerator bounds: 0 to 255.\r\nVanilla KH1FM bounds: 3 to 14.\r\nExtreme custom values are untested.", statLabelHandles[0], editStrength)
	registerStatHelp("MP\r\nAdds to or subtracts from Sora's maximum MP. In KH1, max MP is both your casting resource and a major part of magic/summon power, so higher MP can also make magic stronger.\r\n\r\nGenerator bounds: -128 to 127.\r\nVanilla KH1FM bounds: -2 to +3.\r\nExtreme custom values are untested.", statLabelHandles[1], editMP)
	registerStatHelp("CRIT RATE\r\nMultiplies the critical-hit chance of attacks that are allowed to crit. x1 is the normal Keyblade multiplier, x0.5 halves it, x2 doubles it, and Always is the special Wishing Star behavior.\r\n\r\nGenerator bounds: x0 to x9.95, or Always.\r\nVanilla KH1FM bounds: x0.1, x0.5, x1, x2, or Always.", statLabelHandles[2], editCritRate)
	registerStatHelp("CRIT BONUS\r\nAdds the Keyblade's critical hit bonus to the damage calculation when a critical hit lands. 0 adds no bonus on a crit, 16 adds +16 to that critical attack's power before KH1 calculates final damage.\r\n\r\nGenerator bounds: 0 to 255.\r\nVanilla KH1FM bounds: 0 to 16.\r\nExtreme custom values are untested.", statLabelHandles[3], editCritBonus)
	registerStatHelp("RECOIL\r\nA hidden defensive/guarding stat. The game adds 40 to the weapon's recoil value and compares it with the blocked attack's recoil. Higher recoil can allow faster guard recovery and Counterattack when the comparison succeeds.\r\n\r\nGenerator bounds: 0 to 255.\r\nVanilla KH1FM bounds: 1, 30, 60, or 90.", statLabelHandles[4], editRecoil)
	registerStatHelp("REACH\r\nControls the Keyblade's gameplay reach/weapon length. In this KH1FM table, more-negative values are longer. Example: Wishing Star -3.30 (short), Kingdom Key -3.70 (medium), Ultima Weapon -5.60 (long).\r\n\r\nVanilla KH1FM bounds: -3.30 to -5.60.\r\nNo hard custom bound is known; extreme values are untested.", statLabelHandles[5], editReach, lblReachHint)

	addLabel("Global Options:", 760, 105, 200, 28)
	chkMatch = addCheck(idMatch, "Match Keyblade Lengths", 780, 134, 300, 34)
	chkCustom = addCheck(idCustom, "Custom Keyblade Stats", 780, 172, 300, 34)
	setChecked(chkMatch, true)
	lblReplacementList = addLabel("Replacement List", 760, 220, 220, 28)
	listQueue = createControl(WS_EX_CLIENTEDGE, "LISTBOX", "", WS_CHILD|WS_VISIBLE|WS_TABSTOP|WS_BORDER|WS_VSCROLL|LBS_NOTIFY, 760, 250, 418, 245, mainWnd, idQueue)
	btnRemove = addButton(idRemove, "Remove", 760, 505, 95, 30)
	btnClear = addButton(idClear, "Clear All", 865, 505, 95, 30)
	btnDeselect = addButton(idDeselect, "Deselect", 970, 505, 105, 30)

	lblModName = addLabel("Mod name:", 760, 555, 95, 28)
	editModName = addEdit(0, 855, 552, 323, 31)
	setText(editModName, settings.DefaultModName)
	lblAuthor = addLabel("Author:", 760, 592, 95, 28)
	editAuthor = addEdit(0, 855, 589, 323, 31)
	setText(editAuthor, settings.DefaultAuthor)
	lblSavePath = addLabel("", 760, 624, 418, 26)
	btnChangeSave = addButton(idChangeSave, "Change Save Path...", 760, 648, 155, 34)
	btnGenerate = addButton(idGenerate, "Generate OpenKH Mod", 925, 648, 253, 34)
	updateSavePathLabel()

	// Explicit keyboard focus order. Raw Win32 top-level windows do not perform
	// dialog-style Tab traversal automatically, so keep a conventional order here.
	// Disabled controls (for example stat fields when Custom Stats is off, or Reach
	// while Match Length is enabled) are skipped automatically. Shift+Tab reverses.
	tabOrder = []syscall.Handle{
		btnSetup, btnPrepare, btnOpenLog,
		targetButton, sourceButton, chkMatch, chkCustom,
		editStrength, editMP, editCritRate, editCritBonus, editRecoil, editReach,
		btnAdd, listQueue, btnRemove, btnClear, btnDeselect,
		editModName, editAuthor, btnChangeSave, btnGenerate,
	}

	lblFooter = addLabel("v"+appVersion+"  |  External source packs: place pack JSON + asset folders in Packs, then restart.", 22, 692, 650, 28)
	lblBusyStatus = createControl(0, "STATIC", "", WS_CHILD|WS_BORDER|SS_LEFT, 690, 690, 488, 28, mainWnd, 0)

	targetSel = 0
	sourceSel = 0
	updateSelectorButtons()
	refreshBaseData()
	updateInfo()
	procSetTimer.Call(uintptr(mainWnd), 1, 100, 0)

	var rc RECT
	if ok, _, _ := procGetClientRect.Call(uintptr(mainWnd), uintptr(unsafe.Pointer(&rc))); ok != 0 {
		baseClientW = unscalePx(rc.Right - rc.Left)
		baseClientH = unscalePx(rc.Bottom - rc.Top)
	}
}

func layoutResizableControls(clientWPhysical, clientHPhysical int32) {
	if listQueue == 0 {
		return
	}
	clientW, clientH := unscalePx(clientWPhysical), unscalePx(clientHPhysical)
	if baseClientW == 0 || baseClientH == 0 {
		baseClientW, baseClientH = clientW, clientH
	}
	// The left two configuration columns stay fixed. The right replacement pane
	// grows with the window, and the list takes all additional vertical space.
	rightX := int32(760)
	rightMargin := int32(26)
	rightW := clientW - rightX - rightMargin
	if rightW < 300 {
		rightW = 300
	}

	// Keep the setup buttons right-aligned and generously sized so their labels
	// remain readable at normal and high-DPI scaling.
	headerRight := clientW - 26
	logW, prepareW, setupW, gap := int32(185), int32(145), int32(120), int32(8)
	logX := headerRight - logW
	prepareX := logX - gap - prepareW
	setupX := prepareX - gap - setupW
	moveControl(btnOpenLog, logX, 52, logW, 30)
	moveControl(btnPrepare, prepareX, 52, prepareW, 30)
	moveControl(btnSetup, setupX, 52, setupW, 30)
	moveControl(lblConfig, 660, 24, clientW-686, 22)

	extraH := clientH - baseClientH
	if extraH < 0 {
		extraH = 0
	}
	listH := int32(245) + extraH
	moveControl(listQueue, rightX, 250, rightW, listH)
	moveControl(lblReplacementList, rightX, 220, 220, 28)

	belowY := int32(505) + extraH
	moveControl(btnRemove, rightX, belowY, 95, 30)
	moveControl(btnClear, rightX+105, belowY, 95, 30)
	moveControl(btnDeselect, rightX+210, belowY, 105, 30)

	modY := int32(555) + extraH
	authorY := int32(592) + extraH
	saveLabelY := int32(624) + extraH
	genY := int32(648) + extraH
	moveControl(lblModName, rightX, modY, 95, 28)
	moveControl(editModName, rightX+95, modY-3, rightW-95, 31)
	moveControl(lblAuthor, rightX, authorY, 95, 28)
	moveControl(editAuthor, rightX+95, authorY-3, rightW-95, 31)
	moveControl(lblSavePath, rightX, saveLabelY, rightW, 26)
	changeW := int32(155)
	moveControl(btnChangeSave, rightX, genY, changeW, 34)
	moveControl(btnGenerate, rightX+changeW+10, genY, rightW-changeW-10, 34)
	moveControl(lblFooter, 22, 692+extraH, 650, 28)
	busyX := int32(690)
	busyW := clientW - busyX - 26
	if busyW < 250 {
		busyW = 250
	}
	moveControl(lblBusyStatus, busyX, 690+extraH, busyW, 28)
}

func wndProc(hwnd syscall.Handle, msg uint32, wParam, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			reportRecoveredPanic(fmt.Sprintf("wndProc msg=0x%X wParam=0x%X lParam=0x%X", msg, wParam, lParam), r)
			procPostQuitMessage.Call(1)
			ret = 0
		}
	}()
	return wndProcImpl(hwnd, msg, wParam, lParam)
}

func wndProcImpl(hwnd syscall.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_CREATE:
		mainWnd = hwnd
		createUI()
		return 0
	case WM_GETMINMAXINFO:
		if lParam != 0 {
			mmi := (*MINMAXINFO)(unsafe.Pointer(lParam))
			mmi.PtMinTrackSize.X = scalePx(1220)
			mmi.PtMinTrackSize.Y = scalePx(780)
		}
		return 0
	case WM_SIZE:
		clientW := int32(loword(lParam))
		clientH := int32(hiword(lParam))
		layoutResizableControls(clientW, clientH)
		return 0
	case WM_DRAWITEM:
		if lParam != 0 {
			dis := (*DRAWITEMSTRUCT)(unsafe.Pointer(lParam))
			if _, ok := checkStates[dis.HwndItem]; ok {
				drawLargeCheckbox(dis)
				return 1
			}
		}
	case WM_CTLCOLORSTATIC:
		if syscall.Handle(lParam) == lblBusyStatus && lblBusyStatus != 0 {
			procSetTextColor.Call(wParam, 0x000000FF)
			procSetBkMode.Call(wParam, TRANSPARENT)
			brush, _, _ := procGetSysColorBrush.Call(COLOR_WINDOW)
			return brush
		}
	case WM_TIMER:
		if wParam == 1 {
			updateStatHelpHover()
		}
		return 0
	case WM_DPICHANGED:
		newDPI := uint32(loword(wParam))
		if newDPI >= 96 && newDPI != currentDPI {
			logEvent("DPI changed: %d -> %d", currentDPI, newDPI)
			currentDPI = newDPI
			recreateFont()
			rescaleAllControls()
		}
		if lParam != 0 {
			rc := (*RECT)(unsafe.Pointer(lParam))
			procSetWindowPos.Call(uintptr(hwnd), 0, uintptr(rc.Left), uintptr(rc.Top), uintptr(rc.Right-rc.Left), uintptr(rc.Bottom-rc.Top), 0x0004|0x0010)
		}
		return 0
	case WM_COMMAND:
		id := int(loword(wParam))
		notify := hiword(wParam)
		logEvent("WM_COMMAND begin: id=%d notify=%d targetSel=%d sourceSel=%d match=%v custom=%v queue=%d", id, notify, targetSel, sourceSel, isChecked(chkMatch), isChecked(chkCustom), len(queue))
		defer logEvent("WM_COMMAND end: id=%d", id)
		switch id {
		case idTargetButton:
			if notify == BN_CLICKED {
				if idx := showKeybladeMenu(targetButton, targets, targetSel); idx >= 0 {
					targetSel = idx
					updateSelectorButtons()
					updateInfo()
				}
			}
		case idSourceButton:
			if notify == BN_CLICKED {
				if idx := showKeybladeMenu(sourceButton, sources, sourceSel); idx >= 0 {
					sourceSel = idx
					updateSelectorButtons()
					updateInfo()
				}
			}
		case idMatch:
			if notify == BN_CLICKED {
				toggleChecked(chkMatch)
				updateInfo()
			}
		case idCustom:
			if notify == BN_CLICKED {
				toggleChecked(chkCustom)
				if isChecked(chkCustom) {
					if t := selectedTarget(); t != nil {
						if st, e := getStats(t); e == nil {
							populateStatEdits(st)
						}
					}
				}
				updateInfo()
			}
		case idReach:
			if notify == EN_CHANGE {
				updateReachHint()
			}
		case idAdd:
			if notify == BN_CLICKED {
				addOrUpdateQueue()
			}
		case idRemove:
			if notify == BN_CLICKED {
				removeSelected()
			}
		case idClear:
			if notify == BN_CLICKED {
				clearQueue()
			}
		case idDeselect:
			if notify == BN_CLICKED {
				deselectQueue()
			}
		case idGenerate:
			if notify == BN_CLICKED {
				generateClicked()
			}
		case idChangeSave:
			if notify == BN_CLICKED {
				changeSavePath()
			}
		case idSetup:
			if notify == BN_CLICKED {
				runSetup()
			}
		case idPrepare:
			if notify == BN_CLICKED {
				prepareRaw()
			}
		case idOpenLog:
			if notify == BN_CLICKED {
				openDebugLog()
			}
		case idQueue:
			if notify == LBN_SELCHANGE {
				idx := selectedQueueIndex()
				if idx >= 0 && idx < len(queue) {
					loadQueueItem(idx)
				}
			}
		}
		return 0
	case WM_CLOSE:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case WM_DESTROY:
		procKillTimer.Call(uintptr(hwnd), 1)
		if font != 0 {
			procDeleteObject.Call(uintptr(font))
		}
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

func loadQueueItem(i int) {
	q := queue[i]
	for idx, k := range targets {
		if k.ID == q.TargetID {
			targetSel = idx
			break
		}
	}
	for idx, k := range sources {
		if k.ID == q.SourceID {
			sourceSel = idx
			break
		}
	}
	updateSelectorButtons()
	setChecked(chkMatch, q.MatchLength)
	setChecked(chkCustom, q.CustomStats)
	if q.CustomStats {
		populateStatEdits(q.Stats)
	}
	setText(btnAdd, "Update Replacement")
	updateInfo()
}

func enableHighDPI() {
	// PER_MONITOR_AWARE_V2 = (DPI_AWARENESS_CONTEXT)-4. Opting in before any
	// HWND/common-dialog creation prevents Windows from bitmap-scaling the app
	// and Explorer dialogs, which was the source of the fuzzy 4K appearance.
	ctxPerMonitorAwareV2 := ^uintptr(3)
	if r, _, _ := procSetProcessDpiAwarenessContext.Call(ctxPerMonitorAwareV2); r == 0 {
		procSetProcessDPIAware.Call() // fallback for older Windows builds
	}
	if d, _, _ := procGetDpiForSystem.Call(); d >= 96 {
		currentDPI = uint32(d)
	}
	logEvent("High-DPI awareness enabled; system DPI=%d", currentDPI)
}
func initFont() {
	name := utf16("Segoe UI")
	height := -scalePx(17)
	r, _, _ := procCreateFontW.Call(uintptr(height), 0, 0, 0, uintptr(400), 0, 0, 0, uintptr(1), 0, 0, 0, 0, uintptr(unsafe.Pointer(name)))
	font = syscall.Handle(r)
}
func recreateFont() {
	old := font
	initFont()
	for hwnd := range controlLayouts {
		send(hwnd, WM_SETFONT, uintptr(font), 1)
	}
	if old != 0 {
		procDeleteObject.Call(uintptr(old))
	}
}

func main() {
	// Win32 windows, message queues, and COM are thread-affine. Keep the GUI on
	// one OS thread for its entire lifetime. Earlier builds did not do this, which
	// could make repeated control interaction unstable as Go moved the goroutine.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	exe, e := os.Executable()
	if e != nil {
		return
	}
	appRoot = filepath.Dir(exe)
	initCrashLogging()
	enableHighDPI()
	defer func() {
		if r := recover(); r != nil {
			reportRecoveredPanic("main", r)
		}
	}()
	logEvent("GUI OS thread locked")
	procCoInitializeEx.Call(0, COINIT_APARTMENTTHREADED)
	loadSettings()
	logEvent("Settings loaded: OpenKhRoot=%s GameDt=%s", settings.OpenKhRoot, settings.GameDt)
	if e = loadDefinitions(); e != nil {
		logEvent("Definition load failed: %v", e)
		messageBox(0, e.Error(), "KH1 Keyblade Generator - Fatal Error", MB_OK|MB_ICONERROR)
		return
	}
	h, _, _ := procGetModuleHandleW.Call(0)
	hInstance = syscall.Handle(h)
	initFont()
	className := utf16("KH1KeybladeGeneratorWindow")
	icon, _, _ := procLoadIconW.Call(0, IDI_APPLICATION)
	cursor, _, _ := procLoadCursorW.Call(0, IDC_ARROW)
	wc := WNDCLASSEX{CbSize: uint32(unsafe.Sizeof(WNDCLASSEX{})), Style: CS_HREDRAW | CS_VREDRAW, LpfnWndProc: syscall.NewCallback(wndProc), HInstance: hInstance, HIcon: syscall.Handle(icon), HCursor: syscall.Handle(cursor), HbrBackground: syscall.Handle(COLOR_WINDOW + 1), LpszClassName: className, HIconSm: syscall.Handle(icon)}
	if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		messageBox(0, "Could not register application window class.", "KH1 Keyblade Generator", MB_OK|MB_ICONERROR)
		return
	}
	title := utf16("KH1 Keyblade Generator v" + appVersion)
	r, _, _ := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)), WS_OVERLAPPEDWINDOW, uintptr(scalePx(80)), uintptr(scalePx(60)), uintptr(scalePx(1220)), uintptr(scalePx(780)), 0, 0, uintptr(hInstance), 0)
	if r == 0 {
		messageBox(0, "Could not create the application window.", "KH1 Keyblade Generator", MB_OK|MB_ICONERROR)
		return
	}
	mainWnd = syscall.Handle(r)
	procShowWindow.Call(uintptr(mainWnd), SW_SHOW)
	procUpdateWindow.Call(uintptr(mainWnd))
	logEvent("Main window created: hwnd=0x%X", uintptr(mainWnd))
	if previousCrashDetected && lastCrashPath != "" {
		messageBox(mainWnd, "The previous generator session appears to have ended unexpectedly.\r\n\r\nIts preserved crash report is:\r\n"+lastCrashPath+"\r\n\r\nUse 'Open Last Crash Report' to open it in Notepad and copy/paste it into ChatGPT.", "KH1 Keyblade Generator - Previous Crash Detected", MB_OK|MB_ICONERROR)
	}
	if !fileExists(filepath.Join(settings.OpenKhRoot, "btltbl.bin")) || !dirExists(filepath.Join(settings.OpenKhRoot, "remastered")) {
		showInfo("The generator needs your KH1 extraction paths before it can build mods.\r\n\r\nClick Data Setup... to configure them.")
	}
	var msg MSG
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			logEvent("Message loop ended: ret=%d", int32(ret))
			break
		}
		if handleTabNavigation(&msg) {
			continue
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	markCleanExit()
}
