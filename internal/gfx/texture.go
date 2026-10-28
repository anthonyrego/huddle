package gfx

// Adapted from toybox/pkg/renderer/texture.go (createTextureFromPixels):
// format is a parameter so the same path uploads R8 glyph atlases.

import (
	"fmt"

	"github.com/Zyko0/go-sdl3/sdl"
)

// UploadTexture creates a sampled 2D texture and fills it with pixels, which
// must be tightly packed rows in the given format (RGBA8 or R8).
func UploadTexture(device *sdl.GPUDevice, format sdl.GPUTextureFormat, width, height uint32, pixels []byte) (*sdl.GPUTexture, error) {
	tex, err := device.CreateTexture(&sdl.GPUTextureCreateInfo{
		Type:              sdl.GPU_TEXTURETYPE_2D,
		Format:            format,
		Width:             width,
		Height:            height,
		LayerCountOrDepth: 1,
		NumLevels:         1,
		Usage:             sdl.GPU_TEXTUREUSAGE_SAMPLER,
	})
	if err != nil {
		return nil, fmt.Errorf("create texture: %w", err)
	}
	if err := UpdateTexture(device, tex, 0, 0, width, height, pixels); err != nil {
		device.ReleaseTexture(tex)
		return nil, err
	}
	return tex, nil
}

// UpdateTexture replaces a region of tex with pixels.
func UpdateTexture(device *sdl.GPUDevice, tex *sdl.GPUTexture, x, y, width, height uint32, pixels []byte) error {
	transfer, err := device.CreateTransferBuffer(&sdl.GPUTransferBufferCreateInfo{
		Usage: sdl.GPU_TRANSFERBUFFERUSAGE_UPLOAD,
		Size:  uint32(len(pixels)),
	})
	if err != nil {
		return fmt.Errorf("create transfer buffer: %w", err)
	}
	defer device.ReleaseTransferBuffer(transfer)

	ptr, err := device.MapTransferBuffer(transfer, false)
	if err != nil {
		return fmt.Errorf("map transfer buffer: %w", err)
	}
	copy(mapped(ptr, len(pixels)), pixels)
	device.UnmapTransferBuffer(transfer)

	cmd, err := device.AcquireCommandBuffer()
	if err != nil {
		return fmt.Errorf("acquire command buffer: %w", err)
	}
	pass := cmd.BeginCopyPass()
	pass.UploadToGPUTexture(
		&sdl.GPUTextureTransferInfo{TransferBuffer: transfer, PixelsPerRow: width, RowsPerLayer: height},
		&sdl.GPUTextureRegion{Texture: tex, X: x, Y: y, W: width, H: height, D: 1},
		false,
	)
	pass.End()
	return cmd.Submit()
}

// Uploader replaces textures' pixels again and again, a video's frames,
// through one transfer buffer that grows to fit and is cycled on every
// map, so none waits on a copy still in flight. Adapted from reel's
// internal/gfx/texture.go: besides reel's Upload (one region, on its own
// command buffer), Add and Record put several regions through one map of
// the buffer, on the frame's command buffer.
type Uploader struct {
	device  *sdl.GPUDevice
	staging stagingBuffer
	parts   []upload
}

// upload is one region waiting for Record.
type upload struct {
	tex             *sdl.GPUTexture
	w, h, rowPixels uint32
	pixels          []byte
	offset          uint32
}

// NewUploader returns an Uploader for device's textures.
func NewUploader(device *sdl.GPUDevice) *Uploader { return &Uploader{device: device} }

// Upload replaces a region of tex with pixels whose rows are rowPixels
// apart, on a command buffer of its own.
func (u *Uploader) Upload(tex *sdl.GPUTexture, x, y, width, height, rowPixels uint32, pixels []byte) error {
	dst, err := u.staging.mapFor(u.device, uint32(len(pixels)))
	if err != nil {
		return err
	}
	copy(dst, pixels)
	u.device.UnmapTransferBuffer(u.staging.buf)
	cmd, err := u.device.AcquireCommandBuffer()
	if err != nil {
		return fmt.Errorf("acquire command buffer: %w", err)
	}
	pass := cmd.BeginCopyPass()
	pass.UploadToGPUTexture(
		&sdl.GPUTextureTransferInfo{TransferBuffer: u.staging.buf, PixelsPerRow: rowPixels, RowsPerLayer: height},
		&sdl.GPUTextureRegion{Texture: tex, X: x, Y: y, W: width, H: height, D: 1},
		true,
	)
	pass.End()
	return cmd.Submit()
}

// Add queues the whole of tex (w×h) to be replaced with pixels, whose rows
// are rowPixels apart, by the next Record. pixels must stay as they are
// until then.
func (u *Uploader) Add(tex *sdl.GPUTexture, w, h, rowPixels uint32, pixels []byte) {
	u.parts = append(u.parts, upload{tex: tex, w: w, h: h, rowPixels: rowPixels, pixels: pixels})
}

// Pending is how many regions wait for Record.
func (u *Uploader) Pending() int { return len(u.parts) }

// Record copies every region Add queued into the transfer buffer (one map)
// and records their uploads on cmd in one copy pass. The textures are
// cycled, so a frame still drawing from one keeps its old pixels. The
// queue is empty after, whether it worked or not.
func (u *Uploader) Record(cmd *sdl.GPUCommandBuffer) error {
	defer u.drop() // the pixels' references go too
	if len(u.parts) == 0 {
		return nil
	}
	var total uint32
	for i := range u.parts {
		u.parts[i].offset = total
		// 16-byte aligned offsets suit every backend's copy rules.
		total += (uint32(len(u.parts[i].pixels)) + 15) &^ 15
	}
	dst, err := u.staging.mapFor(u.device, total)
	if err != nil {
		return err
	}
	for _, p := range u.parts {
		copy(dst[p.offset:], p.pixels)
	}
	u.device.UnmapTransferBuffer(u.staging.buf)
	pass := cmd.BeginCopyPass()
	for _, p := range u.parts {
		pass.UploadToGPUTexture(
			&sdl.GPUTextureTransferInfo{TransferBuffer: u.staging.buf, Offset: p.offset, PixelsPerRow: p.rowPixels, RowsPerLayer: p.h},
			&sdl.GPUTextureRegion{Texture: p.tex, W: p.w, H: p.h, D: 1},
			true,
		)
	}
	pass.End()
	return nil
}

// drop empties the queue without uploading.
func (u *Uploader) drop() {
	clear(u.parts)
	u.parts = u.parts[:0]
}

// Release lets the transfer buffer go.
func (u *Uploader) Release() { u.staging.release(u.device) }
