// Copied from an earlier project of the author's.
package gfx

import (
	"fmt"
	"unsafe"

	"github.com/Zyko0/go-sdl3/sdl"
	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/text"
)

// TextTuning adjusts glyph coverage in the shader (see quad.frag.hlsl).
// The zero value is not valid; start from DefaultTextTuning.
type TextTuning struct {
	// Gamma pushes coverage toward linear-light blending for the text's
	// luminance: thinner dark-on-light, bolder light-on-dark. 1 is off.
	Gamma float32
	// Contrast k steepens edges: a*(k+1)/(a*k+1). 0 is off.
	Contrast float32
	// Darken raises coverage to 1/Darken, like stem darkening. 1 is off.
	Darken float32
}

// DefaultTextTuning approximates macOS's default text weight (CoreText with
// font smoothing on): fitted by comparing total ink against CoreText renders
// of the same fonts, light and dark, at 1x and 2x (the viewer's M1 spike).
var DefaultTextTuning = TextTuning{Gamma: 1.5, Contrast: 0.5, Darken: 1.4}

// Renderer draws Batches with the uber-shader into any color target. It
// mirrors the glyph cache's atlas pages in GPU textures and uploads only
// their dirty regions each frame.
type Renderer struct {
	device    *sdl.GPUDevice
	pipelines map[sdl.GPUTextureFormat]*sdl.GPUGraphicsPipeline

	linear *sdl.GPUSampler
	white  *sdl.GPUTexture
	images map[draw.ImageID]*sdl.GPUTexture

	// Text is the glyph coverage tuning pushed to the shader each frame.
	Text TextTuning

	glyphs    *text.GlyphCache
	grayPages []*sdl.GPUTexture
	colorPage []*sdl.GPUTexture
	graySize  int // page size the GPU pages were made for
	colorSize int
	staging   stagingBuffer // atlas uploads
	pending   []atlasUpload

	instances *sdl.GPUBuffer
	transfer  stagingBuffer // instance uploads
	capacity  uint32        // instance buffer size, in instances

	// UploadBytes is the atlas bytes uploaded by the last Prepare.
	UploadBytes int

	video videoPass // video frames as images (video.go)
}

// stagingBuffer is an upload transfer buffer that grows and is cycled on
// every map, so the CPU never waits on a copy still in flight.
type stagingBuffer struct {
	buf  *sdl.GPUTransferBuffer
	size uint32
}

func (s *stagingBuffer) mapFor(device *sdl.GPUDevice, n uint32) ([]byte, error) {
	if n > s.size {
		size := max(s.size*2, 64<<10)
		for size < n {
			size *= 2
		}
		if s.buf != nil {
			device.ReleaseTransferBuffer(s.buf)
		}
		var err error
		s.buf, err = device.CreateTransferBuffer(&sdl.GPUTransferBufferCreateInfo{Usage: sdl.GPU_TRANSFERBUFFERUSAGE_UPLOAD, Size: size})
		if err != nil {
			s.buf, s.size = nil, 0
			return nil, fmt.Errorf("create transfer buffer: %w", err)
		}
		s.size = size
	}
	ptr, err := device.MapTransferBuffer(s.buf, true)
	if err != nil {
		return nil, fmt.Errorf("map transfer buffer: %w", err)
	}
	return mapped(ptr, int(n)), nil
}

func (s *stagingBuffer) release(device *sdl.GPUDevice) {
	if s.buf != nil {
		device.ReleaseTransferBuffer(s.buf)
		s.buf, s.size = nil, 0
	}
}

type atlasUpload struct {
	tex    *sdl.GPUTexture
	page   *text.Page
	atlas  *text.Atlas
	region text.Region
	offset uint32
}

// NewRenderer creates the sampler and fallback texture. Pipelines are built
// lazily per target format.
func NewRenderer(device *sdl.GPUDevice) (*Renderer, error) {
	r := &Renderer{
		device:    device,
		pipelines: map[sdl.GPUTextureFormat]*sdl.GPUGraphicsPipeline{},
		images:    map[draw.ImageID]*sdl.GPUTexture{},
		Text:      DefaultTextTuning,
	}
	var err error
	clamp := sdl.GPU_SAMPLERADDRESSMODE_CLAMP_TO_EDGE
	// Linear everywhere: glyph quads sit on whole pixels, so their samples
	// land on texel centers and read back exactly; under a GPU scale they
	// filter instead of shimmering. Atlas gutters stop neighbors bleeding in.
	r.linear, err = device.CreateSampler(&sdl.GPUSamplerCreateInfo{
		MinFilter: sdl.GPU_FILTER_LINEAR, MagFilter: sdl.GPU_FILTER_LINEAR,
		MipmapMode:   sdl.GPU_SAMPLERMIPMAPMODE_NEAREST,
		AddressModeU: clamp, AddressModeV: clamp, AddressModeW: clamp,
	})
	if err != nil {
		return nil, fmt.Errorf("create sampler: %w", err)
	}
	r.white, err = UploadTexture(device, sdl.GPU_TEXTUREFORMAT_R8G8B8A8_UNORM, 1, 1, []byte{255, 255, 255, 255})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// SetGlyphCache makes the renderer mirror c's atlases. Batches that draw text
// must use the same cache.
func (r *Renderer) SetGlyphCache(c *text.GlyphCache) { r.glyphs = c }

// SetImage registers the texture drawn for an image id. A nil texture removes it.
func (r *Renderer) SetImage(id draw.ImageID, t *sdl.GPUTexture) {
	if t == nil {
		delete(r.images, id)
		return
	}
	r.images[id] = t
}

// premultiplied blends premultiplied color over the target. Adapted from
// toybox's initUIPipeline, switched to premultiplied alpha: the fragment
// shaders output premultiplied color.
var premultiplied = sdl.GPUColorTargetBlendState{
	EnableBlend:         true,
	SrcColorBlendfactor: sdl.GPU_BLENDFACTOR_ONE,
	DstColorBlendfactor: sdl.GPU_BLENDFACTOR_ONE_MINUS_SRC_ALPHA,
	ColorBlendOp:        sdl.GPU_BLENDOP_ADD,
	SrcAlphaBlendfactor: sdl.GPU_BLENDFACTOR_ONE,
	DstAlphaBlendfactor: sdl.GPU_BLENDFACTOR_ONE_MINUS_SRC_ALPHA,
	AlphaBlendOp:        sdl.GPU_BLENDOP_ADD,
}

func (r *Renderer) pipeline(format sdl.GPUTextureFormat) (*sdl.GPUGraphicsPipeline, error) {
	if p := r.pipelines[format]; p != nil {
		return p, nil
	}
	vert, err := LoadShader(r.device, "quad.vert", 0, 1, 0, 0)
	if err != nil {
		return nil, err
	}
	defer r.device.ReleaseShader(vert)
	frag, err := LoadShader(r.device, "quad.frag", 3, 1, 0, 0)
	if err != nil {
		return nil, err
	}
	defer r.device.ReleaseShader(frag)

	p, err := r.device.CreateGraphicsPipeline(&sdl.GPUGraphicsPipelineCreateInfo{
		VertexShader:   vert,
		FragmentShader: frag,
		PrimitiveType:  sdl.GPU_PRIMITIVETYPE_TRIANGLELIST,
		VertexInputState: sdl.GPUVertexInputState{
			VertexBufferDescriptions: []sdl.GPUVertexBufferDescription{{
				Slot:      0,
				Pitch:     instanceSize,
				InputRate: sdl.GPU_VERTEXINPUTRATE_INSTANCE,
			}},
			VertexAttributes: instanceAttributes,
		},
		RasterizerState: sdl.GPURasterizerState{FillMode: sdl.GPU_FILLMODE_FILL, CullMode: sdl.GPU_CULLMODE_NONE},
		TargetInfo: sdl.GPUGraphicsPipelineTargetInfo{
			ColorTargetDescriptions: []sdl.GPUColorTargetDescription{{Format: format, BlendState: premultiplied}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create pipeline: %w", err)
	}
	r.pipelines[format] = p
	return p, nil
}

// syncPages creates GPU textures for atlas pages that don't have one yet.
// If the atlas was replaced with a different page size, the old textures
// are released first. A new texture's contents are undefined; only used
// shelves are ever sampled, and each uploads its full width when opened.
func (r *Renderer) syncPages(a *text.Atlas, texs *[]*sdl.GPUTexture, size *int, format sdl.GPUTextureFormat) error {
	if *size != a.PageSize || len(*texs) > len(a.Pages) {
		for _, t := range *texs {
			r.device.ReleaseTexture(t)
		}
		*texs = (*texs)[:0]
		*size = a.PageSize
	}
	for len(*texs) < len(a.Pages) {
		t, err := r.device.CreateTexture(&sdl.GPUTextureCreateInfo{
			Type:              sdl.GPU_TEXTURETYPE_2D,
			Format:            format,
			Width:             uint32(a.PageSize),
			Height:            uint32(a.PageSize),
			LayerCountOrDepth: 1,
			NumLevels:         1,
			Usage:             sdl.GPU_TEXTUREUSAGE_SAMPLER,
		})
		if err != nil {
			return fmt.Errorf("create atlas page: %w", err)
		}
		*texs = append(*texs, t)
	}
	return nil
}

// Prepare records this frame's uploads on cmd in a copy pass: the dirty
// regions of every atlas page, then the batch's instances. Call Draw after
// it on the same command buffer. On error nothing is recorded and the dirty
// regions stay dirty for the next frame.
func (r *Renderer) Prepare(cmd *sdl.GPUCommandBuffer, b *Batch) error {
	// Video frames first (their own passes): a failure there keeps the
	// pictures the images had, and the rest of the frame goes on.
	r.video.err = r.prepareVideo(cmd)
	r.pending = r.pending[:0]
	r.UploadBytes = 0
	var total uint32
	if c := r.glyphs; c != nil {
		for _, a := range []struct {
			atlas  *text.Atlas
			texs   *[]*sdl.GPUTexture
			size   *int
			format sdl.GPUTextureFormat
		}{
			{c.Gray, &r.grayPages, &r.graySize, sdl.GPU_TEXTUREFORMAT_R8_UNORM},
			{c.Color, &r.colorPage, &r.colorSize, sdl.GPU_TEXTUREFORMAT_R8G8B8A8_UNORM},
		} {
			if err := r.syncPages(a.atlas, a.texs, a.size, a.format); err != nil {
				return err
			}
			for i, p := range a.atlas.Pages {
				tex, atlas := (*a.texs)[i], a.atlas
				p.Dirty(func(reg text.Region) {
					r.pending = append(r.pending, atlasUpload{tex: tex, page: p, atlas: atlas, region: reg, offset: total})
					// Keep offsets 16-byte aligned for every backend's copy rules.
					total += (uint32(reg.W*reg.H*a.atlas.BPP) + 15) &^ 15
				})
			}
		}
	}
	var atlasData []byte
	if total > 0 {
		dst, err := r.staging.mapFor(r.device, total)
		if err != nil {
			return err
		}
		for _, u := range r.pending {
			a, page := u.atlas, u.page
			row, stride := u.region.W*a.BPP, a.PageSize*a.BPP
			for y := range u.region.H {
				src := (u.region.Y+y)*stride + u.region.X*a.BPP
				copy(dst[int(u.offset)+y*row:], page.Pix[src:src+row])
			}
		}
		r.device.UnmapTransferBuffer(r.staging.buf)
		atlasData = dst
		r.UploadBytes = int(total)
	}

	n := uint32(len(b.Instances))
	if n > r.capacity {
		capacity := max(r.capacity*2, 1024)
		for capacity < n {
			capacity *= 2
		}
		if r.instances != nil {
			r.device.ReleaseBuffer(r.instances)
		}
		var err error
		r.instances, err = r.device.CreateBuffer(&sdl.GPUBufferCreateInfo{Usage: sdl.GPU_BUFFERUSAGE_VERTEX, Size: capacity * instanceSize})
		if err != nil {
			r.instances, r.capacity = nil, 0
			return fmt.Errorf("create instance buffer: %w", err)
		}
		r.capacity = capacity
	}
	size := n * instanceSize
	if n > 0 {
		dst, err := r.transfer.mapFor(r.device, size)
		if err != nil {
			return err
		}
		copy(dst, unsafe.Slice((*byte)(unsafe.Pointer(&b.Instances[0])), size))
		r.device.UnmapTransferBuffer(r.transfer.buf)
	}
	if atlasData == nil && n == 0 {
		return nil
	}

	pass := cmd.BeginCopyPass()
	// The uploads are recorded: only now may the pages forget what was dirty.
	if c := r.glyphs; c != nil {
		for _, a := range []*text.Atlas{c.Gray, c.Color} {
			for _, p := range a.Pages {
				p.ClearDirty()
			}
		}
	}
	for _, u := range r.pending {
		pass.UploadToGPUTexture(
			&sdl.GPUTextureTransferInfo{TransferBuffer: r.staging.buf, Offset: u.offset, PixelsPerRow: uint32(u.region.W), RowsPerLayer: uint32(u.region.H)},
			&sdl.GPUTextureRegion{Texture: u.tex, X: uint32(u.region.X), Y: uint32(u.region.Y), W: uint32(u.region.W), H: uint32(u.region.H), D: 1},
			false,
		)
	}
	if n > 0 {
		// Cycling lets the driver hand us a fresh buffer while the previous
		// frame's draw may still be reading this one.
		pass.UploadToGPUBuffer(
			&sdl.GPUTransferBufferLocation{TransferBuffer: r.transfer.buf},
			&sdl.GPUBufferRegion{Buffer: r.instances, Size: size},
			true,
		)
	}
	pass.End()
	return nil
}

// frameUniforms is the vertex stage's cbuffer Frame.
type frameUniforms struct {
	viewport  [4]float32
	transform [4]float32
}

// Draw renders a prepared batch into target (pw×ph pixels, given format),
// clearing to clear first.
func (r *Renderer) Draw(cmd *sdl.GPUCommandBuffer, target *sdl.GPUTexture, format sdl.GPUTextureFormat, pw, ph int, clear draw.Color, b *Batch) error {
	pipe, err := r.pipeline(format)
	if err != nil {
		return err
	}
	pass := cmd.BeginRenderPass([]sdl.GPUColorTargetInfo{{
		Texture:    target,
		ClearColor: fcolor(clear),
		LoadOp:     sdl.GPU_LOADOP_CLEAR,
		StoreOp:    sdl.GPU_STOREOP_STORE,
	}}, nil)
	defer pass.End()
	r.drawSegments(cmd, pass, pipe, pw, ph, b, b.segs)
	return nil
}

func fcolor(c draw.Color) sdl.FColor {
	return sdl.FColor{R: float32(c.R) / 255, G: float32(c.G) / 255, B: float32(c.B) / 255, A: float32(c.A) / 255}
}

// drawSegments records segs of b (prepared) in pass with the uber pipeline.
func (r *Renderer) drawSegments(cmd *sdl.GPUCommandBuffer, pass *sdl.GPURenderPass, pipe *sdl.GPUGraphicsPipeline, pw, ph int, b *Batch, segs []segment) {
	if len(b.Instances) == 0 || len(segs) == 0 {
		return
	}
	pass.BindGraphicsPipeline(pipe)
	tuning := [4]float32{r.Text.Gamma, r.Text.Contrast, r.Text.Darken, 0}
	cmd.PushFragmentUniformData(0, unsafe.Slice((*byte)(unsafe.Pointer(&tuning)), unsafe.Sizeof(tuning)))
	pass.BindVertexBuffers([]sdl.GPUBufferBinding{{Buffer: r.instances}})

	u := frameUniforms{viewport: [4]float32{float32(pw), float32(ph), 0, 0}}
	var lastXform GPUTransform
	pushed := false
	var bound [3]*sdl.GPUTexture
	for _, s := range segs {
		if s.count == 0 || s.scissor.w <= 0 || s.scissor.h <= 0 {
			continue
		}
		if !pushed || s.xform != lastXform {
			u.transform = [4]float32{s.xform.Scale, s.xform.Scale, s.xform.X, s.xform.Y}
			cmd.PushVertexUniformData(0, unsafe.Slice((*byte)(unsafe.Pointer(&u)), unsafe.Sizeof(u)))
			lastXform, pushed = s.xform, true
		}
		pass.SetScissor(&sdl.Rect{X: s.scissor.x, Y: s.scissor.y, W: s.scissor.w, H: s.scissor.h})
		want := [3]*sdl.GPUTexture{r.white, r.white, r.white}
		if s.glyphPage >= 0 && int(s.glyphPage) < len(r.grayPages) {
			want[0] = r.grayPages[s.glyphPage]
		}
		if s.colorPage >= 0 && int(s.colorPage) < len(r.colorPage) {
			want[1] = r.colorPage[s.colorPage]
		}
		if s.imageBound {
			if img := r.images[s.image]; img != nil {
				want[2] = img
			}
		}
		if want != bound {
			pass.BindFragmentSamplers([]sdl.GPUTextureSamplerBinding{
				{Texture: want[0], Sampler: r.linear},
				{Texture: want[1], Sampler: r.linear},
				{Texture: want[2], Sampler: r.linear},
			})
			bound = want
		}
		pass.DrawPrimitives(6, s.count, 0, s.start)
	}
}

// Render uploads b and draws it into target: Prepare then Draw.
func (r *Renderer) Render(cmd *sdl.GPUCommandBuffer, target *sdl.GPUTexture, format sdl.GPUTextureFormat, pw, ph int, clear draw.Color, b *Batch) error {
	if err := r.Prepare(cmd, b); err != nil {
		return err
	}
	return r.Draw(cmd, target, format, pw, ph, clear, b)
}

// Destroy releases GPU resources owned by the renderer, including atlas
// pages (not registered images).
func (r *Renderer) Destroy() {
	r.destroyVideo()
	for _, p := range r.pipelines {
		r.device.ReleaseGraphicsPipeline(p)
	}
	if r.instances != nil {
		r.device.ReleaseBuffer(r.instances)
	}
	r.transfer.release(r.device)
	r.staging.release(r.device)
	for _, t := range r.grayPages {
		r.device.ReleaseTexture(t)
	}
	for _, t := range r.colorPage {
		r.device.ReleaseTexture(t)
	}
	r.device.ReleaseSampler(r.linear)
	r.device.ReleaseTexture(r.white)
}
