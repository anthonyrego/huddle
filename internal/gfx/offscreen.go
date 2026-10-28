package gfx

import (
	"fmt"
	"image"

	"github.com/Zyko0/go-sdl3/sdl"
	"github.com/anthonyrego/huddle/internal/draw"
)

// OffscreenFormat is the color format for offscreen targets.
const OffscreenFormat = sdl.GPU_TEXTUREFORMAT_R8G8B8A8_UNORM

// RenderImage draws b into a new pw×ph texture and reads it back. It needs no
// window; call platform.Init(platform.Background) first so no Dock icon
// appears. Must run on the main thread.
func RenderImage(device *sdl.GPUDevice, r *Renderer, pw, ph int, clear draw.Color, b *Batch) (*image.RGBA, error) {
	return renderImage(device, pw, ph, func(cmd *sdl.GPUCommandBuffer, tex *sdl.GPUTexture) error {
		return r.Render(cmd, tex, OffscreenFormat, pw, ph, clear, b)
	})
}

// renderImage records into a new pw×ph target and reads it back.
func renderImage(device *sdl.GPUDevice, pw, ph int, record func(*sdl.GPUCommandBuffer, *sdl.GPUTexture) error) (*image.RGBA, error) {
	tex, err := device.CreateTexture(&sdl.GPUTextureCreateInfo{
		Type:              sdl.GPU_TEXTURETYPE_2D,
		Format:            OffscreenFormat,
		Width:             uint32(pw),
		Height:            uint32(ph),
		LayerCountOrDepth: 1,
		NumLevels:         1,
		Usage:             sdl.GPU_TEXTUREUSAGE_COLOR_TARGET | sdl.GPU_TEXTUREUSAGE_SAMPLER,
	})
	if err != nil {
		return nil, fmt.Errorf("create target: %w", err)
	}
	defer device.ReleaseTexture(tex)

	size := uint32(pw * ph * 4)
	download, err := device.CreateTransferBuffer(&sdl.GPUTransferBufferCreateInfo{
		Usage: sdl.GPU_TRANSFERBUFFERUSAGE_DOWNLOAD,
		Size:  size,
	})
	if err != nil {
		return nil, fmt.Errorf("create download buffer: %w", err)
	}
	defer device.ReleaseTransferBuffer(download)

	cmd, err := device.AcquireCommandBuffer()
	if err != nil {
		return nil, fmt.Errorf("acquire command buffer: %w", err)
	}
	if err := record(cmd, tex); err != nil {
		cmd.Cancel()
		return nil, err
	}
	pass := cmd.BeginCopyPass()
	pass.DownloadFromGPUTexture(
		&sdl.GPUTextureRegion{Texture: tex, W: uint32(pw), H: uint32(ph), D: 1},
		&sdl.GPUTextureTransferInfo{TransferBuffer: download, PixelsPerRow: uint32(pw), RowsPerLayer: uint32(ph)},
	)
	pass.End()

	fence, err := cmd.SubmitAndAcquireFence()
	if err != nil {
		return nil, fmt.Errorf("submit: %w", err)
	}
	defer device.ReleaseFence(fence)
	if err := device.WaitForFences(true, []*sdl.GPUFence{fence}); err != nil {
		return nil, fmt.Errorf("wait for GPU: %w", err)
	}

	ptr, err := device.MapTransferBuffer(download, false)
	if err != nil {
		return nil, fmt.Errorf("map download buffer: %w", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, pw, ph))
	copy(img.Pix, mapped(ptr, int(size)))
	device.UnmapTransferBuffer(download)
	return img, nil
}
