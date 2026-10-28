// The video conversion pass's vertex stage: one triangle that covers the
// target, with UV running 0 to 1 across it (top left to bottom right). No
// vertex buffer; the vertex id makes the corners.

struct Output
{
    [[vk::location(0)]] float2 UV : TEXCOORD0;
    float4 Position : SV_Position;
};

Output main(uint id : SV_VertexID)
{
    float2 uv = float2((id << 1) & 2, id & 2);
    Output output;
    output.UV = uv;
    output.Position = float4(uv.x * 2.0 - 1.0, 1.0 - uv.y * 2.0, 0.0, 1.0);
    return output;
}
