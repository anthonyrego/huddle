// The video conversion pass: one NV12 frame, uploaded as two textures (the
// brightness plane as R8, the half-size plane of interleaved Cb and Cr as
// R8G8), to RGBA. BT.709, video range (16-235 brightness, 16-240 colour),
// the same sums as video.Frame.RGBA. Src picks the part of the frame shown:
// a crop to the tile's shape, and a negative width mirrors it.

Texture2D<float4> Luma : register(t0, space2);
SamplerState LumaSampler : register(s0, space2);
Texture2D<float4> Chroma : register(t1, space2);
SamplerState ChromaSampler : register(s1, space2);

cbuffer Crop : register(b0, space3)
{
    float4 Src; // u0, v0, du, dv: the frame's UV at the target's left, top, and the span across it
};

struct Input
{
    [[vk::location(0)]] float2 UV : TEXCOORD0;
};

float4 main(Input input) : SV_Target0
{
    float2 uv = Src.xy + input.UV * Src.zw;
    float y = Luma.Sample(LumaSampler, uv).r;
    float2 c = Chroma.Sample(ChromaSampler, uv).rg;
    float l = 1.164383 * (y - 16.0 / 255.0);
    float cb = c.x - 128.0 / 255.0;
    float cr = c.y - 128.0 / 255.0;
    float3 rgb = float3(
        l + 1.792741 * cr,
        l - 0.213249 * cb - 0.532909 * cr,
        l + 2.112402 * cb);
    return float4(saturate(rgb), 1.0);
}
