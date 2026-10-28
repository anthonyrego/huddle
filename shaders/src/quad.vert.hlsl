// Instanced quad for the uber-shader. One instance = one rect, glyph, image,
// stroke segment or filled triangle. All coordinates are target pixels, y
// down. See internal/gfx/instance.go for the matching Go struct; locations
// must stay in sync with it.

cbuffer Frame : register(b0, space1)
{
    float4 Viewport;  // xy = target size in pixels
    // Per-segment transform applied on the GPU, in pixels: p' = p*x + zw.
    // Identity (1, 1, 0, 0) for display lists, whose transforms are baked in
    // on the CPU so glyphs snap to pixels. Uniform scale only (x == y).
    float4 Transform;
};

struct Input
{
    [[vk::location(0)]] float4 Rect : TEXCOORD0;        // x, y, w, h
    [[vk::location(1)]] float4 UV : TEXCOORD1;          // u0, v0, u1, v1
    [[vk::location(2)]] float4 Color : TEXCOORD2;       // straight RGBA
    [[vk::location(3)]] float4 BorderColor : TEXCOORD3; // straight RGBA
    [[vk::location(4)]] float4 Radii : TEXCOORD4;       // tl, tr, br, bl
    [[vk::location(5)]] float4 Params : TEXCOORD5;      // border width, expand, kind, softness
    uint VertexID : SV_VertexID;
};

struct Output
{
    [[vk::location(0)]] float2 Local : TEXCOORD0;
    [[vk::location(1)]] float2 HalfSize : TEXCOORD1;
    [[vk::location(2)]] float2 UV : TEXCOORD2;
    [[vk::location(3)]] float4 Color : TEXCOORD3;
    [[vk::location(4)]] float4 BorderColor : TEXCOORD4;
    [[vk::location(5)]] float4 Radii : TEXCOORD5;
    [[vk::location(6)]] float4 Params : TEXCOORD6;
    // Path kinds (5 stroke, 6 triangle) measure in screen pixels against the
    // fragment's SV_Position, so neighboring instances that share a point
    // evaluate it bit for bit alike: P0 and P1 carry that geometry, flat.
    [[vk::location(7)]] nointerpolation float4 P0 : TEXCOORD7;
    [[vk::location(8)]] nointerpolation float4 P1 : TEXCOORD8;
    float4 Position : SV_Position;
};

static const float2 Corners[6] = {
    float2(0.0, 0.0), float2(1.0, 0.0), float2(0.0, 1.0),
    float2(1.0, 0.0), float2(1.0, 1.0), float2(0.0, 1.0),
};

Output main(Input input)
{
    float2 corner = Corners[input.VertexID];
    float s = Transform.x;
    // Expand is in target pixels; divide it out so antialiasing and blur
    // stay one screen pixel wide under a GPU scale.
    float expand = input.Params.y / s;
    float2 origin = input.Rect.xy - expand;
    float2 size = input.Rect.zw + 2.0 * expand;
    float2 pos = origin + corner * size;

    Output output;
    // The SDF runs in screen pixels: scale everything the fragment stage
    // measures (offsets, half size, radii, border, softness).
    output.Local = (pos - (input.Rect.xy + input.Rect.zw * 0.5)) * s;
    output.HalfSize = input.Rect.zw * 0.5 * s;
    output.UV = lerp(input.UV.xy, input.UV.zw, corner);
    output.Color = input.Color;
    output.BorderColor = input.BorderColor;
    output.Radii = input.Radii * s;
    output.Params = float4(input.Params.x * s, input.Params.y, input.Params.z, input.Params.w * s);
    // Stroke: Radii holds the segment's end points and UV the joint
    // tangents. Triangle: Radii holds two corners and UV.xy the third.
    // Points are absolute instance pixels; map them to the screen here.
    output.P0 = input.Radii * s + Transform.zwzw;
    output.P1 = input.UV;
    if (int(input.Params.z + 0.5) == 6)
    {
        output.P1 = float4(input.UV.xy * s + Transform.zw, input.UV.zw);
    }
    float2 screen = pos * Transform.xy + Transform.zw;
    float2 ndc = screen / Viewport.xy * 2.0 - 1.0;
    output.Position = float4(ndc.x, -ndc.y, 0.0, 1.0);
    return output;
}
