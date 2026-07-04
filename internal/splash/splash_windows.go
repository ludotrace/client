//go:build windows

package splash

import (
	"image"
	"log/slog"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// show creates and runs the heartbeat splash on Windows. It is non-blocking:
// all Win32 work happens on a dedicated, OS-locked goroutine that pumps its own
// message loop and self-destructs after one 2x heartbeat cycle (~2.1s). Any
// failure (frame decode, window creation) is logged and swallowed — the handle
// still finishes, so the caller is never blocked or failed by the splash.
//
// The window is a borderless WS_POPUP, top-most and not on the taskbar, centred
// on the primary monitor. Frames are pre-rendered PNGs (see gen/main.go),
// decoded to top-down BGRA DIBs and blitted with StretchDIBits — no HBITMAP
// lifecycle to leak, and pure-Go/CGO-free, matching the client's Windows build
// (CGO_ENABLED=0) and its existing raw-Win32 usage (autostart, DPAPI fallback).
//
// This mirrors the pure-syscall style already in the client rather than pulling
// in a GUI toolkit, keeping the "dumb by design / low footprint" ethos: the
// goroutine exits and every resource (brush, class, DIBs) is released when the
// splash closes, so the goleak footprint tests stay green (Show is never called
// from tests).
func show() *Handle {
	h := &Handle{done: make(chan struct{})}
	go func() {
		defer h.finish()
		if err := runSplash(); err != nil {
			slog.Warn("splash: not shown", "err", err)
		}
	}()
	return h
}

// --- Win32 plumbing ---------------------------------------------------------

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procUnregisterClassW = user32.NewProc("UnregisterClassW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procBeginPaint       = user32.NewProc("BeginPaint")
	procEndPaint         = user32.NewProc("EndPaint")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procSetTimer         = user32.NewProc("SetTimer")
	procKillTimer        = user32.NewProc("KillTimer")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")

	procStretchDIBits    = gdi32.NewProc("StretchDIBits")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject     = gdi32.NewProc("DeleteObject")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

const (
	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080

	swShow = 5

	wmDestroy = 0x0002
	wmPaint   = 0x000F
	wmTimer   = 0x0113

	csHRedraw = 0x0002
	csVRedraw = 0x0001

	idcArrow = 32512

	smCXScreen = 0
	smCYScreen = 1

	biRGB        = 0
	dibRGBColors = 0
	srcCopy      = 0x00CC0020

	timerID = 1
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type msg struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type rect struct{ left, top, right, bottom int32 }

type paintStruct struct {
	hdc         windows.Handle
	fErase      int32
	rcPaint     rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

// splashState is the per-run state the window procedure needs. Exactly one
// splash exists per process launch (Show is called once, before the tray), so a
// single guarded global is sufficient and avoids threading state through the C
// callback via window user-data.
type splashState struct {
	frames [][]byte // top-down BGRA DIB bytes, one per frame
	header bitmapInfoHeader
	w, h   int32
	idx    int
	hwnd   windows.Handle
}

var (
	activeMu sync.Mutex
	active   *splashState
)

func runSplash() error {
	// Win32 requires window creation and its message loop to run on the same
	// thread; lock it so the Go scheduler can't migrate this goroutine.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	frames, _, err := loadFrames()
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return nil
	}

	w := int32(frames[0].Bounds().Dx())
	h := int32(frames[0].Bounds().Dy())

	st := &splashState{
		frames: make([][]byte, len(frames)),
		w:      w,
		h:      h,
		header: bitmapInfoHeader{
			biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			biWidth:       w,
			biHeight:      -h, // negative => top-down rows, matching our buffers
			biPlanes:      1,
			biBitCount:    32,
			biCompression: biRGB,
		},
	}
	for i, f := range frames {
		st.frames[i] = rgbaToBGRA(f)
	}

	activeMu.Lock()
	active = st
	activeMu.Unlock()
	defer func() {
		activeMu.Lock()
		active = nil
		activeMu.Unlock()
	}()

	hInstance, _, _ := procGetModuleHandleW.Call(0)

	className, _ := windows.UTF16PtrFromString("LudoTraceSplashWindow")
	cursor, _, _ := procLoadCursorW.Call(0, uintptr(idcArrow))

	// Void-coloured background brush avoids a white erase flash before the
	// first paint. COLORREF is 0x00BBGGRR: #12002e -> B=0x2e G=0x00 R=0x12.
	brush, _, _ := procCreateSolidBrush.Call(uintptr(0x002e0012))
	defer procDeleteObject.Call(brush)

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         csHRedraw | csVRedraw,
		lpfnWndProc:   windows.NewCallback(wndProc),
		hInstance:     windows.Handle(hInstance),
		hCursor:       windows.Handle(cursor),
		hbrBackground: windows.Handle(brush),
		lpszClassName: className,
	}
	atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		return err // Errno; already-registered would also land here on a re-run
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), hInstance)

	screenW, _, _ := procGetSystemMetrics.Call(uintptr(smCXScreen))
	screenH, _, _ := procGetSystemMetrics.Call(uintptr(smCYScreen))
	x := (int32(screenW) - w) / 2
	y := (int32(screenH) - h) / 2

	title, _ := windows.UTF16PtrFromString("LudoTrace")
	hwnd, _, err := procCreateWindowExW.Call(
		uintptr(wsExTopmost|wsExToolWindow),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsPopup|wsVisible),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		return err
	}
	st.hwnd = windows.Handle(hwnd)

	procShowWindow.Call(hwnd, uintptr(swShow))
	procUpdateWindow.Call(hwnd)

	// Advance one frame per interval; the last WM_TIMER destroys the window.
	// interval*frameCount == cycle (~2.1s) is what makes this a pure 2x
	// time-scale of the locked motion. SetTimer takes milliseconds.
	intervalMs := (cycle / time.Duration(len(frames))) / time.Millisecond
	if intervalMs < 1 {
		intervalMs = 1
	}
	procSetTimer.Call(hwnd, uintptr(timerID), uintptr(intervalMs), 0)

	// Message loop: runs until WM_DESTROY posts WM_QUIT (GetMessage returns 0).
	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return nil
}

// wndProc is the window procedure. It handles frame advance (WM_TIMER), paint
// (WM_PAINT), and teardown (WM_DESTROY); everything else falls through to
// DefWindowProc. State comes from the single active splash.
func wndProc(hwnd windows.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmTimer:
		activeMu.Lock()
		st := active
		activeMu.Unlock()
		if st == nil {
			return 0
		}
		st.idx++
		if st.idx >= len(st.frames) {
			procKillTimer.Call(uintptr(hwnd), uintptr(timerID))
			procDestroyWindow.Call(uintptr(hwnd))
			return 0
		}
		procInvalidateRect.Call(uintptr(hwnd), 0, 0)
		return 0

	case wmPaint:
		activeMu.Lock()
		st := active
		activeMu.Unlock()
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		if st != nil && st.idx < len(st.frames) {
			bits := st.frames[st.idx]
			procStretchDIBits.Call(
				hdc,
				0, 0, uintptr(st.w), uintptr(st.h),
				0, 0, uintptr(st.w), uintptr(st.h),
				uintptr(unsafe.Pointer(&bits[0])),
				uintptr(unsafe.Pointer(&st.header)),
				uintptr(dibRGBColors),
				uintptr(srcCopy),
			)
		}
		procEndPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return ret
}

// rgbaToBGRA converts an RGBA image to a top-down 32-bpp BGRA byte buffer laid
// out for a BI_RGB DIB (memory order B,G,R,X). The DIB header uses a negative
// height so row 0 is the top, matching this buffer.
func rgbaToBGRA(img *image.RGBA) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		srow := img.Pix[y*img.Stride:]
		drow := out[y*w*4:]
		for x := 0; x < w; x++ {
			r := srow[x*4+0]
			g := srow[x*4+1]
			bl := srow[x*4+2]
			drow[x*4+0] = bl
			drow[x*4+1] = g
			drow[x*4+2] = r
			drow[x*4+3] = 0xff
		}
	}
	return out
}
