package gfx

// Adapted from toybox/pkg/window/window.go: HiDPI back buffer, no HDR or
// fullscreen, device created separately so offscreen rendering can share it,
// and no printing to stdout.

import (
	"fmt"

	"github.com/Zyko0/go-sdl3/sdl"
)

// NewDevice creates the GPU device. It needs no window.
func NewDevice(debug bool) (*sdl.GPUDevice, error) {
	d, err := sdl.CreateGPUDevice(ShaderFormats, debug, "")
	if err != nil {
		return nil, fmt.Errorf("create GPU device: %w", err)
	}
	return d, nil
}

// WindowConfig configures NewWindow. Width and Height are logical units.
type WindowConfig struct {
	Title         string
	Width, Height int
	Hidden        bool
	// Overlay makes a window to draw over the desktop: borderless and above
	// other windows, and not resizable by hand. It is created opaque: SDL's
	// GPU API won't claim a window made with SDL_WINDOW_TRANSPARENT, so the
	// platform makes it see-through after the claim (platform.MakeOverlay).
	Overlay bool
}

// Window is an OS window claimed by a GPU device.
type Window struct {
	handle *sdl.Window
	device *sdl.GPUDevice
}

// NewWindow creates a resizable (or overlay), high-pixel-density window and
// claims it for device.
func NewWindow(device *sdl.GPUDevice, cfg WindowConfig) (*Window, error) {
	flags := sdl.WINDOW_RESIZABLE | sdl.WINDOW_HIGH_PIXEL_DENSITY
	if cfg.Overlay {
		flags = sdl.WINDOW_BORDERLESS | sdl.WINDOW_ALWAYS_ON_TOP | sdl.WINDOW_HIGH_PIXEL_DENSITY
	}
	if cfg.Hidden {
		flags |= sdl.WINDOW_HIDDEN
	}
	handle, err := sdl.CreateWindow(cfg.Title, cfg.Width, cfg.Height, flags)
	if err != nil {
		return nil, fmt.Errorf("create window: %w", err)
	}
	if err := device.ClaimWindow(handle); err != nil {
		handle.Destroy()
		return nil, fmt.Errorf("claim window: %w", err)
	}
	return &Window{handle: handle, device: device}, nil
}

// Handle returns the SDL window.
func (w *Window) Handle() *sdl.Window { return w.handle }

// SwapchainFormat is the texture format of the window's swapchain.
func (w *Window) SwapchainFormat() sdl.GPUTextureFormat {
	return w.device.SwapchainTextureFormat(w.handle)
}

// PixelSize is the back buffer size in pixels.
func (w *Window) PixelSize() (int, int) {
	pw, ph, err := w.handle.SizeInPixels()
	if err != nil {
		return 0, 0
	}
	return int(pw), int(ph)
}

// Scale is the display scale used for layout: logical units = pixels / Scale.
// On macOS Retina it is 2; on Windows at 150% it is 1.5 even though the pixel
// density is 1.
func (w *Window) Scale() float32 {
	s, err := w.handle.DisplayScale()
	if err != nil || s <= 0 {
		return 1
	}
	return s
}

// PixelDensity converts window (event) coordinates to pixels.
func (w *Window) PixelDensity() float32 {
	d, err := w.handle.PixelDensity()
	if err != nil || d <= 0 {
		return 1
	}
	return d
}

// Destroy releases the window from the device and destroys it.
func (w *Window) Destroy() {
	w.device.ReleaseWindow(w.handle)
	w.handle.Destroy()
}

// ToLogical converts window (event) coordinates to logical units: event
// coordinates times PixelDensity gives pixels, and pixels over Scale gives
// layout units. On a Mac both factors are 2 and cancel; on Windows at 150%
// density is 1 and scale 1.5.
func (w *Window) ToLogical(x, y float32) (float32, float32) {
	k := w.PixelDensity() / w.Scale()
	return x * k, y * k
}

// DisplayInfo describes the display the window is on.
type DisplayInfo struct {
	Name string
	// RefreshHz is the current mode's refresh rate (0 if unknown). A
	// variable-refresh (ProMotion) panel reports its maximum.
	RefreshHz    float32
	W, H         int32   // mode size in points
	PixelDensity float32 // mode's pixels per point
	ContentScale float32 // the display's UI scale
}

// Display reports the window's current display and mode.
func (w *Window) Display() DisplayInfo {
	id := sdl.GetDisplayForWindow(w.handle)
	var info DisplayInfo
	info.Name, _ = id.Name()
	if m, err := id.CurrentDisplayMode(); err == nil && m != nil {
		info.RefreshHz = m.RefreshRate
		if m.RefreshRateDenominator > 0 {
			info.RefreshHz = float32(m.RefreshRateNumerator) / float32(m.RefreshRateDenominator)
		}
		info.W, info.H, info.PixelDensity = m.W, m.H, m.PixelDensity
	}
	info.ContentScale, _ = id.ContentScale()
	return info
}
