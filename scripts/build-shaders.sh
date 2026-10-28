#!/usr/bin/env bash
# Compiles shaders/src/*.{vert,frag}.hlsl to SPIR-V (Vulkan) and MSL (Metal),
# then records hashes in shaders/manifest.json. Needs: brew install glslang spirv-cross
#
# SDL3 GPU resource layout (see SDL_CreateGPUShader): vertex stage uses
# space0 for textures and space1 for uniform buffers; fragment stage uses
# space2 and space3. Graphics shaders must not use storage buffers: the MSL
# step maps bindings 1:1 (--msl-decoration-binding), which only matches SDL's
# Metal layout without them. Give every stage input/output an explicit
# [[vk::location(n)]].
set -euo pipefail
cd "$(dirname "$0")/../shaders"

for tool in glslang spirv-cross; do
  command -v "$tool" >/dev/null || { echo "missing $tool: brew install glslang spirv-cross" >&2; exit 1; }
done

mkdir -p compiled/spirv compiled/msl
for src in src/*.hlsl; do
  stem=$(basename "$src" .hlsl)          # e.g. quad.vert
  case "$stem" in
    *.vert) stage=vert ;;
    *.frag) stage=frag ;;
    *) echo "skip $src: name must end in .vert.hlsl or .frag.hlsl" >&2; continue ;;
  esac
  glslang -V -D -S "$stage" -e main --target-env vulkan1.0 -o "compiled/spirv/$stem.spv" "$src" >/dev/null
  spirv-cross --msl --msl-version 20100 --msl-decoration-binding "compiled/spirv/$stem.spv" --output "compiled/msl/$stem.msl"
  echo "built $stem"
done

cd ..
GLSLANG_VERSION=$(glslang --version | sed -n 's/^Glslang Version: //p') \
SPIRV_CROSS_VERSION=$(brew list --versions spirv-cross 2>/dev/null | awk '{print $2}') \
  go run ./scripts/shadermanifest write
