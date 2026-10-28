package gfx

import (
	"unsafe"

	"github.com/Zyko0/go-sdl3/sdl"
)

// Kind selects what the uber-shader draws for an instance. Keep in sync with
// shaders/src/quad.frag.hlsl.
type Kind float32

const (
	KindShape      Kind = 0 // rounded rect, optional border
	KindShadow     Kind = 1 // blurred rounded rect
	KindGlyph      Kind = 2 // R8 coverage from the glyph atlas
	KindColorGlyph Kind = 3 // premultiplied RGBA from the color atlas
	KindImage      Kind = 4 // straight-alpha RGBA image
	KindStroke     Kind = 5 // one polyline segment: Radii = end points, UV = joint tangents
	KindTriangle   Kind = 6 // filled triangle: Radii = two corners, UV.xy = the third
)

// Instance is one quad. Positions and sizes are target pixels. The layout
// must match the vertex attributes in pipeline.go and quad.vert.hlsl.
//
// Path kinds reuse the fields: a stroke segment's end points (absolute
// pixels) are in Radii, the tangents of the joints at its ends in UV (zero
// for a free end), its width in Params.x and butt-cap flags in
// BorderColor.R (start) and .G (end). A triangle's corners are in Radii and
// UV.xy, and BorderColor.RGB flags which of its edges (v0v1, v1v2, v2v0)
// are the polygon's outline and get antialiased.
type Instance struct {
	Rect        [4]float32 // x, y, w, h
	UV          [4]float32 // u0, v0, u1, v1
	Color       [4]uint8   // straight RGBA
	BorderColor [4]uint8   // straight RGBA
	Radii       [4]float32 // tl, tr, br, bl
	// Params: border width, expand (px drawn outside Rect for antialiasing
	// and blur), kind, softness.
	Params [4]float32
}

const instanceSize = uint32(unsafe.Sizeof(Instance{}))

var instanceAttributes = []sdl.GPUVertexAttribute{
	{Location: 0, BufferSlot: 0, Format: sdl.GPU_VERTEXELEMENTFORMAT_FLOAT4, Offset: uint32(unsafe.Offsetof(Instance{}.Rect))},
	{Location: 1, BufferSlot: 0, Format: sdl.GPU_VERTEXELEMENTFORMAT_FLOAT4, Offset: uint32(unsafe.Offsetof(Instance{}.UV))},
	{Location: 2, BufferSlot: 0, Format: sdl.GPU_VERTEXELEMENTFORMAT_UBYTE4_NORM, Offset: uint32(unsafe.Offsetof(Instance{}.Color))},
	{Location: 3, BufferSlot: 0, Format: sdl.GPU_VERTEXELEMENTFORMAT_UBYTE4_NORM, Offset: uint32(unsafe.Offsetof(Instance{}.BorderColor))},
	{Location: 4, BufferSlot: 0, Format: sdl.GPU_VERTEXELEMENTFORMAT_FLOAT4, Offset: uint32(unsafe.Offsetof(Instance{}.Radii))},
	{Location: 5, BufferSlot: 0, Format: sdl.GPU_VERTEXELEMENTFORMAT_FLOAT4, Offset: uint32(unsafe.Offsetof(Instance{}.Params))},
}
