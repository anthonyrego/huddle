// Package gfx is the SDL3 GPU renderer. Everything here runs on the main
// (loop) thread; nothing else may touch the GPU.
package gfx

// Adapted from toybox/pkg/shaders/embed.go: DXIL dropped (the device is
// created with SPIRV|MSL only, so Windows runs on Vulkan).

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Zyko0/go-sdl3/sdl"
	"github.com/anthonyrego/huddle/shaders"
)

// ShaderFormats are the formats the device is created with.
const ShaderFormats = sdl.GPU_SHADERFORMAT_SPIRV | sdl.GPU_SHADERFORMAT_MSL

// LoadShader creates a shader from the embedded compiled output for the
// device's backend. name is e.g. "quad.vert"; the stage comes from the name.
func LoadShader(device *sdl.GPUDevice, name string, samplers, uniformBuffers, storageBuffers, storageTextures uint32) (*sdl.GPUShader, error) {
	var stage sdl.GPUShaderStage
	switch {
	case strings.Contains(name, ".vert"):
		stage = sdl.GPU_SHADERSTAGE_VERTEX
	case strings.Contains(name, ".frag"):
		stage = sdl.GPU_SHADERSTAGE_FRAGMENT
	default:
		return nil, fmt.Errorf("shader %q: name must contain .vert or .frag", name)
	}

	var path, entry string
	var format sdl.GPUShaderFormat
	backend := device.ShaderFormats()
	switch {
	case backend&sdl.GPU_SHADERFORMAT_SPIRV != 0:
		path, format, entry = "compiled/spirv/"+name+".spv", sdl.GPU_SHADERFORMAT_SPIRV, "main"
	case backend&sdl.GPU_SHADERFORMAT_MSL != 0:
		path, format, entry = "compiled/msl/"+name+".msl", sdl.GPU_SHADERFORMAT_MSL, "main0"
	default:
		return nil, errors.New("backend supports neither SPIR-V nor MSL")
	}

	code, err := shaders.FS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("shader %q: %w", name, err)
	}
	shader, err := device.CreateGPUShader(&sdl.GPUShaderCreateInfo{
		Code:               code,
		Entrypoint:         entry,
		Format:             format,
		Stage:              stage,
		NumSamplers:        samplers,
		NumUniformBuffers:  uniformBuffers,
		NumStorageBuffers:  storageBuffers,
		NumStorageTextures: storageTextures,
	})
	if err != nil {
		return nil, fmt.Errorf("shader %q: %w", name, err)
	}
	return shader, nil
}
