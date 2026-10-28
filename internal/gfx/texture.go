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
