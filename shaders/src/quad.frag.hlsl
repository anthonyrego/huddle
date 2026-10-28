// Uber-shader fragment stage. Kind (Params.z) selects the primitive:
//   0 shape: rounded rect with optional border (SDF, 1px antialiasing)
//   1 shadow: blurred rounded rect, softness in Params.w
//   2 glyph: coverage from the R8 glyph atlas, tinted by Color
//   3 color glyph: premultiplied RGBA from the color (emoji) atlas
//   4 image: straight-alpha RGBA image, clipped to the rounded rect
//   5 stroke: one segment of a polyline (see strokeDistance)
//   6 triangle: a filled triangle of a tessellated polygon (see triangleCoverage)
// Output is premultiplied alpha.

Texture2D<float4> GlyphAtlas : register(t0, space2);
SamplerState GlyphSampler : register(s0, space2);
Texture2D<float4> ColorAtlas : register(t1, space2);
SamplerState ColorSampler : register(s1, space2);
Texture2D<float4> Image : register(t2, space2);
SamplerState ImageSampler : register(s2, space2);

// Glyph coverage tuning, set per frame (gfx.TextTuning). Blending happens in
// the swapchain's gamma-encoded space, which thins light-on-dark text and
// thickens dark-on-light; these knobs trade that off for crispness.
cbuffer Text : register(b0, space3)
{
    // x: gamma for luminance-aware correction (1 = off). Pushes coverage
    //    toward what linear-light blending would give for this text color.
    // y: contrast k (0 = off): a*(k+1)/(a*k+1), steepens edges.
    // z: darken (1 = off): a^(1/z), a uniform stem-darkening stand-in.
    // w: unused.
    float4 TextParams;
};

struct Input
{
    [[vk::location(0)]] float2 Local : TEXCOORD0;
    [[vk::location(1)]] float2 HalfSize : TEXCOORD1;
    [[vk::location(2)]] float2 UV : TEXCOORD2;
    [[vk::location(3)]] float4 Color : TEXCOORD3;
    [[vk::location(4)]] float4 BorderColor : TEXCOORD4;
    [[vk::location(5)]] float4 Radii : TEXCOORD5;
    [[vk::location(6)]] float4 Params : TEXCOORD6;
    [[vk::location(7)]] nointerpolation float4 P0 : TEXCOORD7;
    [[vk::location(8)]] nointerpolation float4 P1 : TEXCOORD8;
    float4 Pos : SV_Position;
};

// Signed distance to a rounded box centered at the origin, y down.
// r = radii (top-left, top-right, bottom-right, bottom-left).
float sdRoundBox(float2 p, float2 b, float4 r)
{
    float rad = (p.x > 0.0) ? ((p.y > 0.0) ? r.z : r.y) : ((p.y > 0.0) ? r.w : r.x);
    float2 q = abs(p) - b + rad;
    return min(max(q.x, q.y), 0.0) + length(max(q, 0.0)) - rad;
}

float4 premul(float4 c, float a)
{
    return float4(c.rgb * a, a);
}

float glyphCoverage(float a, float3 color)
{
    float g = TextParams.x;
    if (g != 1.0)
    {
        float luma = dot(color, float3(0.2126, 0.7152, 0.0722));
        float inv = 1.0 / g;
        float light = pow(a, inv);              // light text on dark
        float dark = 1.0 - pow(1.0 - a, inv);   // dark text on light
        a = lerp(dark, light, luma);
    }
    float k = TextParams.y;
    a = a * (k + 1.0) / (a * k + 1.0);
    if (TextParams.z != 1.0)
    {
        a = pow(a, 1.0 / TextParams.z);
    }
    return saturate(a);
}

// strokeDistance is the signed distance from pixel center p to the stroke of
// segment a-b (screen pixels), width w. Ends are round (a capsule) unless
// buttA/buttB square them off at the end point, as SVG's butt caps do for
// dashes.
//
// A polyline is one instance per segment. Neighbors would both cover the
// round join between them, and their antialiased edges would add up to a
// heavier fringe there, so each segment keeps only its side of the line
// through the joint across the path (normal to tangent ta at a, tb at b;
// zero for a free end). On its side a segment is also the nearer one, so
// its distance is the whole stroke's. The test runs on the exact pixel
// center and on the shared point and tangent, identical in both instances,
// so every pixel belongs to exactly one segment. It returns a large
// distance for pixels on the other side.
float strokeDistance(float2 p, float2 a, float2 b, float w, float2 ta, float2 tb, bool buttA, bool buttB)
{
    if (dot(p - a, ta) < 0.0)
    {
        return 1e6;
    }
    if (dot(tb, tb) > 0.0 && dot(p - b, tb) >= 0.0)
    {
        return 1e6;
    }
    float2 ba = b - a;
    float len = length(ba);
    float2 dir = len > 0.0 ? ba / len : float2(1.0, 0.0);
    float2 pa = p - a;
    float along = dot(pa, dir);
    float hw = 0.5 * w;
    float perp = abs(dir.x * pa.y - dir.y * pa.x) - hw;
    float d = perp;
    if (buttA)
    {
        d = max(d, -along);
    }
    else if (along < 0.0)
    {
        d = length(pa) - hw;
    }
    if (buttB)
    {
        d = max(d, along - len);
    }
    else if (along > len)
    {
        d = length(p - b) - hw;
    }
    return d;
}

// edgeCoverage is one edge's contribution to a triangle's coverage at p.
// sgn is +1 when the triangle winds counterclockwise in screen space (y
// down), else -1. Exterior edges (the polygon's outline) are antialiased;
// interior edges (diagonals shared with a neighboring triangle) are hard,
// and are evaluated with the end points in a canonical order so both
// triangles compute the same value and a pixel on the edge goes to exactly
// one of them.
float edgeCoverage(float2 a, float2 b, float2 p, float sgn, bool exterior)
{
    bool swap = (b.x < a.x) || (b.x == a.x && b.y < a.y);
    float2 lo = swap ? b : a;
    float2 hi = swap ? a : b;
    float s = swap ? -sgn : sgn;
    float2 e = hi - lo;
    float c = s * (e.x * (p.y - lo.y) - e.y * (p.x - lo.x));
    if (exterior)
    {
        return saturate(0.5 + c / max(length(e), 1e-6));
    }
    if (c > 0.0 || (c == 0.0 && s > 0.0))
    {
        return 1.0;
    }
    return 0.0;
}

float triangleCoverage(float2 p, float2 v0, float2 v1, float2 v2, float3 exterior)
{
    float2 e1 = v1 - v0;
    float2 e2 = v2 - v0;
    float sgn = (e1.x * e2.y - e1.y * e2.x) >= 0.0 ? 1.0 : -1.0;
    float c = edgeCoverage(v0, v1, p, sgn, exterior.x > 0.5);
    c = min(c, edgeCoverage(v1, v2, p, sgn, exterior.y > 0.5));
    c = min(c, edgeCoverage(v2, v0, p, sgn, exterior.z > 0.5));
    return c;
}

float4 main(Input input) : SV_Target0
{
    int kind = int(input.Params.z + 0.5);
    float d = sdRoundBox(input.Local, input.HalfSize, input.Radii);

    if (kind == 0)
    {
        float outer = saturate(0.5 - d);
        float bw = input.Params.x;
        float4 c = input.Color;
        if (bw > 0.0)
        {
            float inner = saturate(0.5 - (d + bw));
            c = lerp(input.BorderColor, input.Color, inner);
        }
        return premul(c, c.a * outer);
    }
    if (kind == 1)
    {
        float soft = max(input.Params.w, 0.5);
        float a = 1.0 - smoothstep(-soft, soft, d);
        return premul(input.Color, input.Color.a * a);
    }
    if (kind == 2)
    {
        float cov = glyphCoverage(GlyphAtlas.Sample(GlyphSampler, input.UV).r, input.Color.rgb);
        return premul(input.Color, input.Color.a * cov);
    }
    if (kind == 3)
    {
        return ColorAtlas.Sample(ColorSampler, input.UV) * input.Color.a;
    }
    if (kind == 5)
    {
        float sd = strokeDistance(input.Pos.xy, input.P0.xy, input.P0.zw, input.Params.x,
            input.P1.xy, input.P1.zw, input.BorderColor.r > 0.5, input.BorderColor.g > 0.5);
        return premul(input.Color, input.Color.a * saturate(0.5 - sd));
    }
    if (kind == 6)
    {
        float cov = triangleCoverage(input.Pos.xy, input.P0.xy, input.P0.zw, input.P1.xy, input.BorderColor.rgb);
        return premul(input.Color, input.Color.a * cov);
    }
    float4 t = Image.Sample(ImageSampler, input.UV);
    float clip = saturate(0.5 - d);
    return premul(t, t.a * input.Color.a * clip);
}
