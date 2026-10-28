package gfx

// Video frames as images. SDL's GPU API has no NV12 texture format, so a
// frame goes up as two textures, its brightness plane as R8 and its
// half-size plane of interleaved Cb and Cr as R8G8, through one Uploader;
// then a pass (shaders/src/nv12.frag.hlsl) converts them into an RGBA
// texture, cropped to the shape it's shown in and mirrored when the frame
// says so. That texture is the image an id draws (draw.List.Image, with
// its rounded corners and opacity) until the next frame replaces it.
// Frames are put between Prepares; Prepare uploads and converts them, so
// the window and RenderImage both draw the newest. Main thread only.

import (
	"fmt"
	"unsafe"

	"github.com/Zyko0/go-sdl3/sdl"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/video"
)

// videoFormat is the converted picture's format.
const videoFormat = sdl.GPU_TEXTUREFORMAT_R8G8B8A8_UNORM

// videoImage is one id's video: the newest frame, until Prepare takes it,
// and its textures.
type videoImage struct {
	id     draw.ImageID
	frame  video.Frame // a copy of the newest frame put
	aspect float32     // crop to this width over height (0: the whole frame)
	queued bool        // frame waits for Prepare
	y, uv  *sdl.GPUTexture
	rgba   *sdl.GPUTexture
	fw, fh int // the planes' size (the frame's)
	ow, oh int // the picture's size, after the crop
}

// videoPass is the conversion pipeline and the frames waiting for it.
type videoPass struct {
	pipe   *sdl.GPUGraphicsPipeline
	up     *Uploader
	images map[draw.ImageID]*videoImage
	queue  []*videoImage
	srcs   [][4]float32 // each queued frame's Src, for the shader
	err    error        // the last Prepare's, if its frames couldn't be converted
}

// PutFrame makes f the next picture image id draws: f is copied now, and
// the next Prepare uploads and converts it. aspect (width over height)
// crops the middle of the frame to that shape, as a tile shows it; 0
// keeps the whole frame. A frame put again before Prepare replaces the
// one waiting.
func (r *Renderer) PutFrame(id draw.ImageID, f video.Frame, aspect float32) {
	if f.W <= 0 || f.H <= 0 {
		return
	}
	v := &r.video
	if v.images == nil {
		v.images = map[draw.ImageID]*videoImage{}
	}
	img := v.images[id]
	if img == nil {
		img = &videoImage{id: id}
		v.images[id] = img
	}
	img.frame.CopyFrom(f)
	img.aspect = aspect
	if !img.queued {
		img.queued = true
		v.queue = append(v.queue, img)
	}
}

// DropFrame forgets image id's video and releases its textures.
func (r *Renderer) DropFrame(id draw.ImageID) {
	v := &r.video
	img := v.images[id]
	if img == nil {
		return
	}
	delete(v.images, id)
	for i, q := range v.queue {
		if q == img {
			v.queue = append(v.queue[:i], v.queue[i+1:]...)
			break
		}
	}
	r.SetImage(id, nil)
	r.releaseVideo(img)
}

// VideoErr is why the last Prepare couldn't convert its frames (nil when
// it could). The images keep the pictures they had.
func (r *Renderer) VideoErr() error { return r.video.err }

// VideoFrames is how many frames wait for the next Prepare.
func (r *Renderer) VideoFrames() int { return len(r.video.queue) }

func (r *Renderer) releaseVideo(img *videoImage) {
	for _, t := range []*sdl.GPUTexture{img.y, img.uv, img.rgba} {
		if t != nil {
			r.device.ReleaseTexture(t)
		}
	}
	img.y, img.uv, img.rgba = nil, nil, nil
}

// crop is the part of a fw×fh frame shown at aspect (0: all of it), as
// the shader's Src (u0, v0, du, dv), and that part's size in pixels.
func crop(fw, fh int, aspect float32, mirror bool) (src [4]float32, w, h int) {
	src = [4]float32{0, 0, 1, 1}
	w, h = fw, fh
	if fa := float32(fw) / float32(fh); aspect > 0 {
		switch {
		case fa > aspect*1.001: // wider: trim the sides
			src[2] = aspect / fa
			src[0] = (1 - src[2]) / 2
			w = max(int(float32(fh)*aspect+0.5), 1)
		case fa < aspect/1.001: // taller: trim the top and bottom
			src[3] = fa / aspect
			src[1] = (1 - src[3]) / 2
			h = max(int(float32(fw)/aspect+0.5), 1)
		}
	}
	if mirror {
		src[0] += src[2]
		src[2] = -src[2]
	}
	return src, w, h
}

func (r *Renderer) videoTexture(format sdl.GPUTextureFormat, w, h int, usage sdl.GPUTextureUsageFlags) (*sdl.GPUTexture, error) {
	t, err := r.device.CreateTexture(&sdl.GPUTextureCreateInfo{
		Type:              sdl.GPU_TEXTURETYPE_2D,
		Format:            format,
		Width:             uint32(w),
		Height:            uint32(h),
		LayerCountOrDepth: 1,
		NumLevels:         1,
		Usage:             usage,
	})
	if err != nil {
		return nil, fmt.Errorf("create video texture: %w", err)
	}
	return t, nil
}

// fit makes img's textures the sizes its frame needs.
func (r *Renderer) fit(img *videoImage, ow, oh int) error {
	f := &img.frame
	if img.y == nil || img.fw != f.W || img.fh != f.H {
		r.releaseVideo(img)
		var err error
		if img.y, err = r.videoTexture(sdl.GPU_TEXTUREFORMAT_R8_UNORM, f.W, f.H, sdl.GPU_TEXTUREUSAGE_SAMPLER); err != nil {
			return err
		}
		if img.uv, err = r.videoTexture(sdl.GPU_TEXTUREFORMAT_R8G8_UNORM, f.W/2, f.H/2, sdl.GPU_TEXTUREUSAGE_SAMPLER); err != nil {
			return err
		}
		img.fw, img.fh = f.W, f.H
	}
	if img.rgba == nil || img.ow != ow || img.oh != oh {
		if img.rgba != nil {
			r.device.ReleaseTexture(img.rgba)
			img.rgba = nil
		}
		var err error
		if img.rgba, err = r.videoTexture(videoFormat, ow, oh, sdl.GPU_TEXTUREUSAGE_COLOR_TARGET|sdl.GPU_TEXTUREUSAGE_SAMPLER); err != nil {
			return err
		}
		img.ow, img.oh = ow, oh
	}
	return nil
}

func (r *Renderer) videoPipeline() (*sdl.GPUGraphicsPipeline, error) {
	if p := r.video.pipe; p != nil {
		return p, nil
	}
	vert, err := LoadShader(r.device, "nv12.vert", 0, 0, 0, 0)
	if err != nil {
		return nil, err
	}
	defer r.device.ReleaseShader(vert)
	frag, err := LoadShader(r.device, "nv12.frag", 2, 1, 0, 0)
	if err != nil {
		return nil, err
	}
	defer r.device.ReleaseShader(frag)
	p, err := r.device.CreateGraphicsPipeline(&sdl.GPUGraphicsPipelineCreateInfo{
		VertexShader:    vert,
		FragmentShader:  frag,
		PrimitiveType:   sdl.GPU_PRIMITIVETYPE_TRIANGLELIST,
		RasterizerState: sdl.GPURasterizerState{FillMode: sdl.GPU_FILLMODE_FILL, CullMode: sdl.GPU_CULLMODE_NONE},
		TargetInfo: sdl.GPUGraphicsPipelineTargetInfo{
			ColorTargetDescriptions: []sdl.GPUColorTargetDescription{{Format: videoFormat}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create video pipeline: %w", err)
	}
	r.video.pipe = p
	return p, nil
}

// prepareVideo records the waiting frames' uploads (one copy pass) and
// their conversions (a render pass each) on cmd, and registers each
// picture as its id's image. On an error nothing is recorded and the
// frames are dropped: the images keep the pictures they had.
func (r *Renderer) prepareVideo(cmd *sdl.GPUCommandBuffer) error {
	v := &r.video
	if len(v.queue) == 0 {
		return nil
	}
	queue := v.queue
	defer func() {
		for _, img := range queue {
			img.queued = false
		}
		clear(v.queue)
		v.queue = v.queue[:0]
	}()
	pipe, err := r.videoPipeline()
	if err != nil {
		return err
	}
	if v.up == nil {
		v.up = NewUploader(r.device)
	}
	v.srcs = v.srcs[:0]
	for _, img := range queue {
		f := &img.frame
		src, ow, oh := crop(f.W, f.H, img.aspect, f.Mirror)
		if err := r.fit(img, ow, oh); err != nil {
			v.up.drop()
			return err
		}
		v.srcs = append(v.srcs, src)
		v.up.Add(img.y, uint32(f.W), uint32(f.H), uint32(f.YStride), f.Y[:f.YStride*f.H])
		v.up.Add(img.uv, uint32(f.W/2), uint32(f.H/2), uint32(f.UVStride/2), f.UV[:f.UVStride*f.H/2])
	}
	if err := v.up.Record(cmd); err != nil {
		return err
	}
	for i, img := range queue {
		pass := cmd.BeginRenderPass([]sdl.GPUColorTargetInfo{{
			Texture: img.rgba,
			LoadOp:  sdl.GPU_LOADOP_DONT_CARE,
			StoreOp: sdl.GPU_STOREOP_STORE,
			Cycle:   true, // a frame still drawing the last picture keeps it
		}}, nil)
		pass.BindGraphicsPipeline(pipe)
		pass.BindFragmentSamplers([]sdl.GPUTextureSamplerBinding{
			{Texture: img.y, Sampler: r.linear},
			{Texture: img.uv, Sampler: r.linear},
		})
		cmd.PushFragmentUniformData(0, unsafe.Slice((*byte)(unsafe.Pointer(&v.srcs[i])), unsafe.Sizeof(v.srcs[i])))
		pass.DrawPrimitives(3, 1, 0, 0)
		pass.End()
		r.SetImage(img.id, img.rgba)
	}
	return nil
}

// destroyVideo releases every video texture, the pipeline and the
// transfer buffer.
func (r *Renderer) destroyVideo() {
	v := &r.video
	for _, img := range v.images {
		r.releaseVideo(img)
	}
	v.images, v.queue = nil, nil
	if v.pipe != nil {
		r.device.ReleaseGraphicsPipeline(v.pipe)
		v.pipe = nil
	}
	if v.up != nil {
		v.up.Release()
	}
}
