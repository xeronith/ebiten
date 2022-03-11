// Copyright 2022 The Ebiten Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package directx

import (
	"fmt"
	"math"

	"github.com/hajimehoshi/ebiten/v2/internal/graphics"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver"
)

const numDescriptorsPerFrame = 256

func operationToBlend(c graphicsdriver.Operation) _D3D12_BLEND {
	switch c {
	case graphicsdriver.Zero:
		return _D3D12_BLEND_ZERO
	case graphicsdriver.One:
		return _D3D12_BLEND_ONE
	case graphicsdriver.SrcAlpha:
		return _D3D12_BLEND_SRC_ALPHA
	case graphicsdriver.DstAlpha:
		return _D3D12_BLEND_DEST_ALPHA
	case graphicsdriver.OneMinusSrcAlpha:
		return _D3D12_BLEND_INV_SRC_ALPHA
	case graphicsdriver.OneMinusDstAlpha:
		return _D3D12_BLEND_INV_DEST_ALPHA
	case graphicsdriver.DstColor:
		return _D3D12_BLEND_DEST_COLOR
	default:
		panic(fmt.Sprintf("directx: invalid operation: %d", c))
	}
}

type builtinPipelineStatesKey struct {
	useColorM     bool
	compositeMode graphicsdriver.CompositeMode
	filter        graphicsdriver.Filter
	address       graphicsdriver.Address
	screen        bool
}

func (k *builtinPipelineStatesKey) defs() ([]_D3D_SHADER_MACRO, error) {
	var defs []_D3D_SHADER_MACRO
	defval := []byte("1\x00")
	if k.useColorM {
		name := []byte("USE_COLOR_MATRIX\x00")
		defs = append(defs, _D3D_SHADER_MACRO{&name[0], &defval[0]})
	}

	switch k.filter {
	case graphicsdriver.FilterNearest:
		name := []byte("FILTER_NEAREST\x00")
		defs = append(defs, _D3D_SHADER_MACRO{&name[0], &defval[0]})
	case graphicsdriver.FilterLinear:
		name := []byte("FILTER_LINEAR\x00")
		defs = append(defs, _D3D_SHADER_MACRO{&name[0], &defval[0]})
	case graphicsdriver.FilterScreen:
		name := []byte("FILTER_SCREEN\x00")
		defs = append(defs, _D3D_SHADER_MACRO{&name[0], &defval[0]})
	default:
		return nil, fmt.Errorf("directx: invalid filter: %d", k.filter)
	}

	switch k.address {
	case graphicsdriver.AddressUnsafe:
		name := []byte("ADDRESS_UNSAFE\x00")
		defs = append(defs, _D3D_SHADER_MACRO{&name[0], &defval[0]})
	case graphicsdriver.AddressClampToZero:
		name := []byte("ADDRESS_CLAMP_TO_ZERO\x00")
		defs = append(defs, _D3D_SHADER_MACRO{&name[0], &defval[0]})
	case graphicsdriver.AddressRepeat:
		name := []byte("ADDRESS_REPEAT\x00")
		defs = append(defs, _D3D_SHADER_MACRO{&name[0], &defval[0]})
	default:
		return nil, fmt.Errorf("directx: invalid address: %d", k.address)
	}

	// Termination
	defs = append(defs, _D3D_SHADER_MACRO{})

	return defs, nil
}

func (k *builtinPipelineStatesKey) source() []byte {
	return []byte(`struct PSInput {
  float4 position : SV_POSITION;
  float2 texcoord : TEXCOORD0;
  float4 color : COLOR;
};

cbuffer ShaderParameter : register(b0) {
  float2 viewport_size;
  float2 source_size;
  float4x4 color_matrix_body;
  float4 color_matrix_translation;
  float4 source_region;

  // This member should be the last not to create a new sector.
  // https://docs.microsoft.com/en-us/windows/win32/direct3dhlsl/dx-graphics-hlsl-packing-rules
  float scale;
}

PSInput VSMain(float2 position : POSITION, float2 tex : TEXCOORD, float4 color : COLOR) {
  // In DirectX, the NDC's Y direction (upward) and the framebuffer's Y direction (downward) don't
  // match. Then, the Y direction must be inverted.
  float4x4 projectionMatrix = {
    2.0 / viewport_size.x, 0, 0, -1,
    0, -2.0 / viewport_size.y, 0, 1,
    0, 0, 1, 0,
    0, 0, 0, 1,
  };

  PSInput result;
  result.position = mul(projectionMatrix, float4(position, 0, 1));
  result.texcoord = tex;
  result.color = float4(color.rgb, 1) * color.a;
  return result;
}

Texture2D tex : register(t0);
SamplerState samp : register(s0);

float euclideanMod(float x, float y) {
  // Assume that y is always positive.
  return x - y * floor(x/y);
}

float2 adjustTexelByAddress(float2 p, float4 source_region) {
#if defined(ADDRESS_CLAMP_TO_ZERO)
  return p;
#endif

#if defined(ADDRESS_REPEAT)
  float2 o = float2(source_region[0], source_region[1]);
  float2 size = float2(source_region[2] - source_region[0], source_region[3] - source_region[1]);
  return float2(euclideanMod((p.x - o.x), size.x) + o.x, euclideanMod((p.y - o.y), size.y) + o.y);
#endif

#if defined(ADDRESS_UNSAFE)
  return p;
#endif
}

float4 PSMain(PSInput input) : SV_TARGET {
#if defined(FILTER_NEAREST)
# if defined(ADDRESS_UNSAFE)
  float4 color = tex.Sample(samp, input.texcoord);
# else
  float4 color;
  float2 pos = adjustTexelByAddress(input.texcoord, source_region);
  if (source_region[0] <= pos.x &&
      source_region[1] <= pos.y &&
      pos.x < source_region[2] &&
      pos.y < source_region[3]) {
    color = tex.Sample(samp, pos);
  } else {
    color = float4(0, 0, 0, 0);
  }
# endif // defined(ADDRESS_UNSAFE)
#endif // defined(FILTER_NEAREST)

#if defined(FILTER_LINEAR)
  float2 pos = input.texcoord;
  float2 texel_size = 1.0 / source_size;

  // Shift 1/512 [texel] to avoid the tie-breaking issue.
  // As all the vertex positions are aligned to 1/16 [pixel], this shiting should work in most cases.
  float2 p0 = pos - (texel_size) / 2.0 + (texel_size / 512.0);
  float2 p1 = pos + (texel_size) / 2.0 + (texel_size / 512.0);

# if !defined(ADDRESS_UNSAFE)
  p0 = adjustTexelByAddress(p0, source_region);
  p1 = adjustTexelByAddress(p1, source_region);
# endif  // !defined(ADDRESS_UNSAFE)

  float4 c0 = tex.Sample(samp, p0);
  float4 c1 = tex.Sample(samp, float2(p1.x, p0.y));
  float4 c2 = tex.Sample(samp, float2(p0.x, p1.y));
  float4 c3 = tex.Sample(samp, p1);

# if !defined(ADDRESS_UNSAFE)
  if (p0.x < source_region[0]) {
    c0 = float4(0, 0, 0, 0);
    c2 = float4(0, 0, 0, 0);
  }
  if (p0.y < source_region[1]) {
    c0 = float4(0, 0, 0, 0);
    c1 = float4(0, 0, 0, 0);
  }
  if (source_region[2] <= p1.x) {
    c1 = float4(0, 0, 0, 0);
    c3 = float4(0, 0, 0, 0);
  }
  if (source_region[3] <= p1.y) {
    c2 = float4(0, 0, 0, 0);
    c3 = float4(0, 0, 0, 0);
  }
# endif  // !defined(ADDRESS_UNSAFE)

  float2 rate = frac(p0 * source_size);
  float4 color = lerp(lerp(c0, c1, rate.x), lerp(c2, c3, rate.x), rate.y);
#endif // defined(FILTER_LINEAR)

#if defined(FILTER_SCREEN)
  float2 pos = input.texcoord;
  float2 texel_size = 1.0 / source_size;
  float2 half_scaled_texel_size = texel_size / 2.0 / scale;

  float2 p0 = pos - half_scaled_texel_size + (texel_size / 512.0);
  float2 p1 = pos + half_scaled_texel_size + (texel_size / 512.0);

  float4 c0 = tex.Sample(samp, p0);
  float4 c1 = tex.Sample(samp, float2(p1.x, p0.y));
  float4 c2 = tex.Sample(samp, float2(p0.x, p1.y));
  float4 c3 = tex.Sample(samp, p1);
  // Texels must be in the source rect, so it is not necessary to check that like linear filter.

  float2 rate_center = float2(1.0, 1.0) - half_scaled_texel_size;
  float2 rate = clamp(((frac(p0 * source_size) - rate_center) * scale) + rate_center, 0.0, 1.0);
  float4 color = lerp(lerp(c0, c1, rate.x), lerp(c2, c3, rate.x), rate.y);
#endif // defined(FILTER_SCREEN)

#if defined(USE_COLOR_MATRIX)
  // Un-premultiply alpha.
  // When the alpha is 0, 1.0 - sign(alpha) is 1.0, which means division does nothing.
  color.rgb /= color.a + (1.0 - sign(color.a));
  // Apply the color matrix or scale.
  color = mul(color_matrix_body, color) + color_matrix_translation;
  // Premultiply alpha
  color.rgb *= color.a;
  // Apply color scale.
  color *= input.color;
  // Clamp the output.
  color.rgb = min(color.rgb, color.a);
  return color;
#elif defined(FILTER_SCREEN)
  return color;
#else
  return input.color * color;
#endif // defined(USE_COLOR_MATRIX)

}`)
}

type pipelineStates struct {
	rootSignature *iD3D12RootSignature

	cache map[builtinPipelineStatesKey]*iD3D12PipelineState

	// builtinShaders is a set of the built-in vertex/pixel shaders that are never released.
	builtinShaders []*iD3DBlob

	shaderDescriptorHeap *iD3D12DescriptorHeap
	shaderDescriptorSize uint32

	samplerDescriptorHeap *iD3D12DescriptorHeap

	lastFrameIndex  int
	constantBuffers [frameCount][]*iD3D12Resource1
}

const numConstantBufferAndSourceTextures = 1 + graphics.ShaderImageNum

func (p *pipelineStates) initialize(device *iD3D12Device) (ferr error) {
	// Create a CBV/SRV/UAV descriptor heap.
	//   5n+0:        constants
	//   5n+m (1<=4): textures
	shaderH, err := device.CreateDescriptorHeap(&_D3D12_DESCRIPTOR_HEAP_DESC{
		Type:           _D3D12_DESCRIPTOR_HEAP_TYPE_CBV_SRV_UAV,
		NumDescriptors: frameCount * numDescriptorsPerFrame * numConstantBufferAndSourceTextures,
		Flags:          _D3D12_DESCRIPTOR_HEAP_FLAG_SHADER_VISIBLE,
		NodeMask:       0,
	})
	if err != nil {
		return err
	}
	p.shaderDescriptorHeap = shaderH
	defer func() {
		if ferr != nil {
			p.shaderDescriptorHeap.Release()
			p.shaderDescriptorHeap = nil
		}
	}()
	p.shaderDescriptorSize = device.GetDescriptorHandleIncrementSize(_D3D12_DESCRIPTOR_HEAP_TYPE_CBV_SRV_UAV)

	samplerH, err := device.CreateDescriptorHeap(&_D3D12_DESCRIPTOR_HEAP_DESC{
		Type:           _D3D12_DESCRIPTOR_HEAP_TYPE_SAMPLER,
		NumDescriptors: 1,
		Flags:          _D3D12_DESCRIPTOR_HEAP_FLAG_SHADER_VISIBLE,
		NodeMask:       0,
	})
	if err != nil {
		return err
	}
	p.samplerDescriptorHeap = samplerH

	device.CreateSampler(&_D3D12_SAMPLER_DESC{
		Filter:         _D3D12_FILTER_MIN_MAG_MIP_POINT,
		AddressU:       _D3D12_TEXTURE_ADDRESS_MODE_WRAP,
		AddressV:       _D3D12_TEXTURE_ADDRESS_MODE_WRAP,
		AddressW:       _D3D12_TEXTURE_ADDRESS_MODE_WRAP,
		ComparisonFunc: _D3D12_COMPARISON_FUNC_NEVER,
		MinLOD:         -math.MaxFloat32,
		MaxLOD:         math.MaxFloat32,
	}, p.samplerDescriptorHeap.GetCPUDescriptorHandleForHeapStart())

	return nil
}

func (p *pipelineStates) useBuiltinGraphicsPipelineState(device *iD3D12Device, commandList *iD3D12GraphicsCommandList, frameIndex int, key builtinPipelineStatesKey, dst *Image, srcs [graphics.ShaderImageNum]*Image, srcRegion graphicsdriver.Region, colorM graphicsdriver.ColorM) error {
	state, ok := p.cache[key]
	if !ok {
		defs, err := key.defs()
		if err != nil {
			return err
		}

		vsh, psh, err := newShader(key.source(), defs)
		if err != nil {
			return err
		}
		// Keep the shaders. These are never released.
		p.builtinShaders = append(p.builtinShaders, vsh, psh)

		s, err := p.newPipelineState(device, vsh, psh, key.compositeMode)
		if err != nil {
			return err
		}
		if p.cache == nil {
			p.cache = map[builtinPipelineStatesKey]*iD3D12PipelineState{}
		}
		p.cache[key] = s
		state = s
	}

	if p.lastFrameIndex != frameIndex {
		p.constantBuffers[frameIndex] = p.constantBuffers[frameIndex][:0]
	}
	p.lastFrameIndex = frameIndex

	idx := len(p.constantBuffers[frameIndex])
	if idx >= numDescriptorsPerFrame*2 {
		return fmt.Errorf("directx: too many constant buffers")
	}

	if cap(p.constantBuffers[frameIndex]) > idx {
		p.constantBuffers[frameIndex] = p.constantBuffers[frameIndex][:idx+1]
	} else {
		p.constantBuffers[frameIndex] = append(p.constantBuffers[frameIndex], nil)
	}

	cb := p.constantBuffers[frameIndex][idx]
	if cb == nil {
		// TODO: What if the buffer size is bigger?
		// Now the maximum is 64 floats (without considering the padding).
		const bufferSize = 256
		var err error
		cb, err = createBuffer(device, bufferSize, _D3D12_HEAP_TYPE_UPLOAD)
		if err != nil {
			return err
		}
		p.constantBuffers[frameIndex][idx] = cb

		h := p.shaderDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
		h.Offset(int32(frameIndex*numDescriptorsPerFrame+numConstantBufferAndSourceTextures*idx), p.shaderDescriptorSize)
		device.CreateConstantBufferView(&_D3D12_CONSTANT_BUFFER_VIEW_DESC{
			BufferLocation: cb.GetGPUVirtualAddress(),
			SizeInBytes:    bufferSize,
		}, h)
	}

	h := p.shaderDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
	h.Offset(int32(frameIndex*numDescriptorsPerFrame+numConstantBufferAndSourceTextures*idx), p.shaderDescriptorSize)
	for _, src := range srcs {
		h.Offset(1, p.shaderDescriptorSize)
		if src == nil {
			continue
		}
		device.CreateShaderResourceView(src.resource(), &_D3D12_SHADER_RESOURCE_VIEW_DESC{
			Format:                  _DXGI_FORMAT_R8G8B8A8_UNORM,
			ViewDimension:           _D3D12_SRV_DIMENSION_TEXTURE2D,
			Shader4ComponentMapping: _D3D12_DEFAULT_SHADER_4_COMPONENT_MAPPING,
			Texture2D: _D3D12_TEX2D_SRV{
				MipLevels: 1,
			},
		}, h)
	}

	// Update the constant buffer.
	r := _D3D12_RANGE{0, 0}
	m, err := cb.Map(0, &r)
	if err != nil {
		return err
	}

	screenWidth, screenHeight := dst.internalSize()
	var srcWidth, srcHeight float32
	if key.filter != graphicsdriver.FilterNearest {
		w, h := srcs[0].internalSize()
		srcWidth = float32(w)
		srcHeight = float32(h)
	}
	var esBody [16]float32
	var esTranslate [4]float32
	colorM.Elements(&esBody, &esTranslate)
	scale := float32(0)
	if key.filter == graphicsdriver.FilterScreen {
		scale = float32(dst.width) / float32(srcs[0].width)
	}
	uniforms := []float32{
		float32(screenWidth),
		float32(screenHeight),
		srcWidth,
		srcHeight,
		esBody[0],
		esBody[1],
		esBody[2],
		esBody[3],
		esBody[4],
		esBody[5],
		esBody[6],
		esBody[7],
		esBody[8],
		esBody[9],
		esBody[10],
		esBody[11],
		esBody[12],
		esBody[13],
		esBody[14],
		esBody[15],
		esTranslate[0],
		esTranslate[1],
		esTranslate[2],
		esTranslate[3],
		srcRegion.X,
		srcRegion.Y,
		srcRegion.X + srcRegion.Width,
		srcRegion.Y + srcRegion.Height,
		scale,
	}
	copyFloat32s(m, uniforms)

	if err := cb.Unmap(0, nil); err != nil {
		return err
	}

	commandList.SetPipelineState(state)

	rs, err := p.ensureRootSignature(device)
	if err != nil {
		return err
	}
	commandList.SetGraphicsRootSignature(rs)

	commandList.SetDescriptorHeaps([]*iD3D12DescriptorHeap{
		p.shaderDescriptorHeap,
		p.samplerDescriptorHeap,
	})

	// Match the indices with rootParams in graphicsPipelineState.
	gh := p.shaderDescriptorHeap.GetGPUDescriptorHandleForHeapStart()
	gh.Offset(int32(frameIndex*numDescriptorsPerFrame+numConstantBufferAndSourceTextures*idx), p.shaderDescriptorSize)
	commandList.SetGraphicsRootDescriptorTable(0, gh)

	// TODO: If a custom shader is used, set the descriptor tables for multiple source textures.
	gh.Offset(1, p.shaderDescriptorSize)
	commandList.SetGraphicsRootDescriptorTable(1, gh)

	commandList.SetGraphicsRootDescriptorTable(2, p.samplerDescriptorHeap.GetGPUDescriptorHandleForHeapStart())

	return nil
}

func (p *pipelineStates) useGraphicsPipelineState(device *iD3D12Device, commandList *iD3D12GraphicsCommandList, frameIndex int, pipelineState *iD3D12PipelineState, dst *Image, srcs [graphics.ShaderImageNum]*Image, srcRegion graphicsdriver.Region) error {
	return nil
}

func (p *pipelineStates) ensureRootSignature(device *iD3D12Device) (rootSignature *iD3D12RootSignature, ferr error) {
	if p.rootSignature != nil {
		return p.rootSignature, nil
	}

	cbv := _D3D12_DESCRIPTOR_RANGE{
		RangeType:                         _D3D12_DESCRIPTOR_RANGE_TYPE_CBV, // b0
		NumDescriptors:                    1,
		BaseShaderRegister:                0,
		RegisterSpace:                     0,
		OffsetInDescriptorsFromTableStart: _D3D12_DESCRIPTOR_RANGE_OFFSET_APPEND,
	}
	srv := _D3D12_DESCRIPTOR_RANGE{
		RangeType:                         _D3D12_DESCRIPTOR_RANGE_TYPE_SRV, // t0
		NumDescriptors:                    graphics.ShaderImageNum,
		BaseShaderRegister:                0,
		RegisterSpace:                     0,
		OffsetInDescriptorsFromTableStart: _D3D12_DESCRIPTOR_RANGE_OFFSET_APPEND,
	}
	sampler := _D3D12_DESCRIPTOR_RANGE{
		RangeType:                         _D3D12_DESCRIPTOR_RANGE_TYPE_SAMPLER, // s0
		NumDescriptors:                    1,
		BaseShaderRegister:                0,
		RegisterSpace:                     0,
		OffsetInDescriptorsFromTableStart: _D3D12_DESCRIPTOR_RANGE_OFFSET_APPEND,
	}

	rootParams := [...]_D3D12_ROOT_PARAMETER{
		{
			ParameterType: _D3D12_ROOT_PARAMETER_TYPE_DESCRIPTOR_TABLE,
			DescriptorTable: _D3D12_ROOT_DESCRIPTOR_TABLE{
				NumDescriptorRanges: 1,
				pDescriptorRanges:   &cbv,
			},
			ShaderVisibility: _D3D12_SHADER_VISIBILITY_ALL,
		},
		{
			ParameterType: _D3D12_ROOT_PARAMETER_TYPE_DESCRIPTOR_TABLE,
			DescriptorTable: _D3D12_ROOT_DESCRIPTOR_TABLE{
				NumDescriptorRanges: 1,
				pDescriptorRanges:   &srv,
			},
			ShaderVisibility: _D3D12_SHADER_VISIBILITY_PIXEL,
		},
		{
			ParameterType: _D3D12_ROOT_PARAMETER_TYPE_DESCRIPTOR_TABLE,
			DescriptorTable: _D3D12_ROOT_DESCRIPTOR_TABLE{
				NumDescriptorRanges: 1,
				pDescriptorRanges:   &sampler,
			},
			ShaderVisibility: _D3D12_SHADER_VISIBILITY_PIXEL,
		},
	}

	// Create a root signature.
	sig, err := d3D12SerializeRootSignature(&_D3D12_ROOT_SIGNATURE_DESC{
		NumParameters:     uint32(len(rootParams)),
		pParameters:       &rootParams[0],
		NumStaticSamplers: 0,
		pStaticSamplers:   nil,
		Flags:             _D3D12_ROOT_SIGNATURE_FLAG_ALLOW_INPUT_ASSEMBLER_INPUT_LAYOUT,
	}, _D3D_ROOT_SIGNATURE_VERSION_1_0)
	if err != nil {
		return nil, err
	}
	defer sig.Release()

	rs, err := device.CreateRootSignature(0, sig.GetBufferPointer(), sig.GetBufferSize())
	if err != nil {
		return nil, err
	}
	defer func() {
		if ferr != nil {
			rootSignature.Release()
		}
	}()

	p.rootSignature = rs

	return p.rootSignature, nil
}

func newShader(source []byte, defs []_D3D_SHADER_MACRO) (vsh, psh *iD3DBlob, ferr error) {
	// Create a shader
	v, err := d3DCompile(source, "shader", defs, nil, "VSMain", "vs_5_0", 0, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if ferr != nil {
			v.Release()
		}
	}()

	p, err := d3DCompile(source, "shader", defs, nil, "PSMain", "ps_5_0", 0, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if ferr != nil {
			p.Release()
		}
	}()

	return v, p, nil
}

func (p *pipelineStates) newPipelineState(device *iD3D12Device, vsh, psh *iD3DBlob, compositeMode graphicsdriver.CompositeMode) (state *iD3D12PipelineState, ferr error) {
	srcOp, dstOp := compositeMode.Operations()
	renderTargetBlendDesc := _D3D12_RENDER_TARGET_BLEND_DESC{
		BlendEnable:           1,
		LogicOpEnable:         0,
		SrcBlend:              operationToBlend(srcOp),
		DestBlend:             operationToBlend(dstOp),
		BlendOp:               _D3D12_BLEND_OP_ADD,
		SrcBlendAlpha:         operationToBlend(srcOp),
		DestBlendAlpha:        operationToBlend(dstOp),
		BlendOpAlpha:          _D3D12_BLEND_OP_ADD,
		LogicOp:               _D3D12_LOGIC_OP_NOOP,
		RenderTargetWriteMask: uint8(_D3D12_COLOR_WRITE_ENABLE_ALL),
	}
	rasterizerDesc := _D3D12_RASTERIZER_DESC{
		FillMode:              _D3D12_FILL_MODE_SOLID,
		CullMode:              _D3D12_CULL_MODE_NONE,
		FrontCounterClockwise: 0,
		DepthBias:             _D3D12_DEFAULT_DEPTH_BIAS,
		DepthBiasClamp:        _D3D12_DEFAULT_DEPTH_BIAS_CLAMP,
		SlopeScaledDepthBias:  _D3D12_DEFAULT_SLOPE_SCALED_DEPTH_BIAS,
		DepthClipEnable:       0,
		MultisampleEnable:     0,
		AntialiasedLineEnable: 0,
		ForcedSampleCount:     0,
		ConservativeRaster:    _D3D12_CONSERVATIVE_RASTERIZATION_MODE_OFF,
	}

	rootSignature, err := p.ensureRootSignature(device)
	if err != nil {
		return nil, err
	}
	defer func() {
		if ferr != nil {
			rootSignature.Release()
		}
	}()

	// Create a pipeline state.
	psoDesc := _D3D12_GRAPHICS_PIPELINE_STATE_DESC{
		pRootSignature: rootSignature,
		VS: _D3D12_SHADER_BYTECODE{
			pShaderBytecode: vsh.GetBufferPointer(),
			BytecodeLength:  vsh.GetBufferSize(),
		},
		PS: _D3D12_SHADER_BYTECODE{
			pShaderBytecode: psh.GetBufferPointer(),
			BytecodeLength:  psh.GetBufferSize(),
		},
		BlendState: _D3D12_BLEND_DESC{
			AlphaToCoverageEnable:  0,
			IndependentBlendEnable: 0,
			RenderTarget: [8]_D3D12_RENDER_TARGET_BLEND_DESC{
				renderTargetBlendDesc,
			},
		},
		SampleMask:      math.MaxUint32,
		RasterizerState: rasterizerDesc,
		InputLayout: _D3D12_INPUT_LAYOUT_DESC{
			pInputElementDescs: &inputElementDescs[0],
			NumElements:        uint32(len(inputElementDescs)),
		},
		PrimitiveTopologyType: _D3D12_PRIMITIVE_TOPOLOGY_TYPE_TRIANGLE,
		NumRenderTargets:      1,
		RTVFormats: [8]_DXGI_FORMAT{
			_DXGI_FORMAT_R8G8B8A8_UNORM,
		},
		SampleDesc: _DXGI_SAMPLE_DESC{
			Count:   1,
			Quality: 0,
		},
	}

	s, err := device.CreateGraphicsPipelineState(&psoDesc)
	if err != nil {
		return nil, err
	}
	return s, nil
}
