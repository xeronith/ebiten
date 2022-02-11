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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/hajimehoshi/ebiten/v2/internal/graphics"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver"
	"github.com/hajimehoshi/ebiten/v2/internal/shaderir"
	"github.com/hajimehoshi/ebiten/v2/internal/shaderir/hlsl"
)

const frameCount = 2

const is64bit = uint64(^uintptr(0)) == ^uint64(0)

// isDirectXAvailable indicates whether DirectX is available or not.
// In 32bit machines, DirectX is not used because
//   1) The functions syscall.Syscall cannot accept 64bit values as one argument
//   2) The struct layouts can be different
var isDirectXAvailable = is64bit && theGraphics.initializeDevice() == nil

var theGraphics Graphics

func Get() *Graphics {
	if !isDirectXAvailable {
		return nil
	}
	return &theGraphics
}

var inputElementDescs []_D3D12_INPUT_ELEMENT_DESC

func init() {
	position := []byte("POSITION\000")
	texcoord := []byte("TEXCOORD\000")
	color := []byte("COLOR\000")
	inputElementDescs = []_D3D12_INPUT_ELEMENT_DESC{
		{
			SemanticName:         &position[0],
			SemanticIndex:        0,
			Format:               _DXGI_FORMAT_R32G32_FLOAT,
			InputSlot:            0,
			AlignedByteOffset:    _D3D12_APPEND_ALIGNED_ELEMENT,
			InputSlotClass:       _D3D12_INPUT_CLASSIFICATION_PER_VERTEX_DATA,
			InstanceDataStepRate: 0,
		},
		{
			SemanticName:         &texcoord[0],
			SemanticIndex:        0,
			Format:               _DXGI_FORMAT_R32G32_FLOAT,
			InputSlot:            0,
			AlignedByteOffset:    _D3D12_APPEND_ALIGNED_ELEMENT,
			InputSlotClass:       _D3D12_INPUT_CLASSIFICATION_PER_VERTEX_DATA,
			InstanceDataStepRate: 0,
		},
		{
			SemanticName:         &color[0],
			SemanticIndex:        0,
			Format:               _DXGI_FORMAT_R32G32B32A32_FLOAT,
			InputSlot:            0,
			AlignedByteOffset:    _D3D12_APPEND_ALIGNED_ELEMENT,
			InputSlotClass:       _D3D12_INPUT_CLASSIFICATION_PER_VERTEX_DATA,
			InstanceDataStepRate: 0,
		},
	}
}

type Graphics struct {
	debug             *iD3D12Debug
	device            *iD3D12Device
	commandQueue      *iD3D12CommandQueue
	rtvDescriptorHeap *iD3D12DescriptorHeap
	rtvDescriptorSize uint32
	renderTargets     [frameCount]*iD3D12Resource1

	fences      [frameCount]*iD3D12Fence
	fenceValues [frameCount]uint64

	// fenceWaitEvent is an event.
	// As all the Graphics functions work in a single thread, only one event is enough for multiple fences.
	fenceWaitEvent windows.Handle

	// drawCommandAllocators are command allocators for a 3D engine (DrawIndexedInstanced).
	// For the word 'engine', see https://docs.microsoft.com/en-us/windows/win32/direct3d12/user-mode-heap-synchronization.
	// The term 'draw' is used instead of '3D' in this package.
	drawCommandAllocators [frameCount]*iD3D12CommandAllocator

	// copyCommandAllocators are command allocators for a copy engine (CopyTextureRegion).
	copyCommandAllocators [frameCount]*iD3D12CommandAllocator

	// drawCommandList is a command list for a 3D engine (DrawIndexedInstanced).
	drawCommandList *iD3D12GraphicsCommandList

	// copyCommandList is a command list for a copy engine (CopyTextureRegion).
	copyCommandList *iD3D12GraphicsCommandList

	// drawCommandList and copyCommandList are exclusive: if one is not empty, the other must be empty.

	vertices [frameCount][]*iD3D12Resource1
	indices  [frameCount][]*iD3D12Resource1

	factory   *iDXGIFactory4
	adapter   *iDXGIAdapter1
	swapChain *iDXGISwapChain4

	window windows.HWND

	frameIndex int

	images         map[graphicsdriver.ImageID]*Image
	screenImage    *Image
	nextImageID    graphicsdriver.ImageID
	disposedImages [frameCount][]*Image

	shaders         map[graphicsdriver.ShaderID]*Shader
	nextShaderID    graphicsdriver.ShaderID
	disposedShaders [frameCount][]*Shader

	debugCommandList *iD3D12DebugCommandList

	vsyncEnabled bool

	pipelineStates
}

func (g *Graphics) initializeDevice() (ferr error) {
	if err := d3d12.Load(); err != nil {
		return err
	}

	// As g's lifetime is the same as the process's lifetime, debug and other objects are never released
	// if this initialization succeeds.

	d, err := d3D12GetDebugInterface()
	if err != nil {
		return err
	}
	g.debug = d
	defer func() {
		if ferr != nil {
			g.debug.Release()
			g.debug = nil
		}
	}()
	g.debug.EnableDebugLayer()

	f, err := createDXGIFactory2(_DXGI_CREATE_FACTORY_DEBUG)
	if err != nil {
		return err
	}
	g.factory = f
	defer func() {
		if ferr != nil {
			g.factory.Release()
			g.factory = nil
		}
	}()

	if useWARP {
		a, err := g.factory.EnumWarpAdapter()
		if err != nil {
			return err
		}

		g.adapter = a
		defer func() {
			if ferr != nil {
				g.adapter.Release()
				g.adapter = nil
			}
		}()
	} else {
		for i := 0; ; i++ {
			a, err := g.factory.EnumAdapters1(uint32(i))
			if errors.Is(err, _DXGI_ERROR_NOT_FOUND) {
				break
			}
			if err != nil {
				return err
			}

			desc, err := a.GetDesc1()
			if err != nil {
				return err
			}
			if desc.Flags&_DXGI_ADAPTER_FLAG_SOFTWARE != 0 {
				a.Release()
				continue
			}
			if err := d3D12CreateDevice(unsafe.Pointer(a), _D3D_FEATURE_LEVEL_11_0, &_IID_ID3D12Device, nil); err != nil {
				a.Release()
				continue
			}
			g.adapter = a
			defer func() {
				if ferr != nil {
					g.adapter.Release()
					g.adapter = nil
				}
			}()
			break
		}
	}

	if g.adapter == nil {
		return errors.New("directx: DirectX 12 is not supported")
	}

	if err := d3D12CreateDevice(unsafe.Pointer(g.adapter), _D3D_FEATURE_LEVEL_11_0, &_IID_ID3D12Device, (*unsafe.Pointer)(unsafe.Pointer(&g.device))); err != nil {
		return err
	}

	return nil
}

func (g *Graphics) Initialize() (ferr error) {
	// Create an event for a fence.
	e, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("directx: CreateEvent failed: %w", err)
	}
	g.fenceWaitEvent = e

	// Create a command queue.
	desc := _D3D12_COMMAND_QUEUE_DESC{
		Type:  _D3D12_COMMAND_LIST_TYPE_DIRECT,
		Flags: _D3D12_COMMAND_QUEUE_FLAG_NONE,
	}
	c, err := g.device.CreateCommandQueue(&desc)
	if err != nil {
		return err
	}
	g.commandQueue = c
	defer func() {
		if ferr != nil {
			g.commandQueue.Release()
			g.commandQueue = nil
		}
	}()

	// Create command allocators.
	for i := 0; i < frameCount; i++ {
		dca, err := g.device.CreateCommandAllocator(_D3D12_COMMAND_LIST_TYPE_DIRECT)
		if err != nil {
			return err
		}
		g.drawCommandAllocators[i] = dca
		defer func(i int) {
			if ferr != nil {
				g.drawCommandAllocators[i].Release()
				g.drawCommandAllocators[i] = nil
			}
		}(i)

		cca, err := g.device.CreateCommandAllocator(_D3D12_COMMAND_LIST_TYPE_DIRECT)
		if err != nil {
			return err
		}
		g.copyCommandAllocators[i] = cca
		defer func(i int) {
			if ferr != nil {
				g.copyCommandAllocators[i].Release()
				g.copyCommandAllocators[i] = nil
			}
		}(i)
	}

	// Create frame fences.
	for i := 0; i < frameCount; i++ {
		f, err := g.device.CreateFence(0, _D3D12_FENCE_FLAG_NONE)
		if err != nil {
			return err
		}
		g.fences[i] = f
		defer func(i int) {
			if ferr != nil {
				g.fences[i].Release()
				g.fences[i] = nil
			}
		}(i)
	}

	// Create command lists.
	dcl, err := g.device.CreateCommandList(0, _D3D12_COMMAND_LIST_TYPE_DIRECT, g.drawCommandAllocators[0], nil)
	if err != nil {
		return err
	}
	g.drawCommandList = dcl
	defer func() {
		if ferr != nil {
			g.drawCommandList.Release()
			g.drawCommandList = nil
		}
	}()

	ccl, err := g.device.CreateCommandList(0, _D3D12_COMMAND_LIST_TYPE_DIRECT, g.copyCommandAllocators[0], nil)
	if err != nil {
		return err
	}
	g.copyCommandList = ccl
	defer func() {
		if ferr != nil {
			g.copyCommandList.Release()
			g.copyCommandList = nil
		}
	}()

	// Close the command list once as this is immediately Reset at Begin.
	if err := g.drawCommandList.Close(); err != nil {
		return err
	}
	if err := g.copyCommandList.Close(); err != nil {
		return err
	}

	// Create a descriptor heap for RTV.
	h, err := g.device.CreateDescriptorHeap(&_D3D12_DESCRIPTOR_HEAP_DESC{
		Type:           _D3D12_DESCRIPTOR_HEAP_TYPE_RTV,
		NumDescriptors: frameCount,
		Flags:          _D3D12_DESCRIPTOR_HEAP_FLAG_NONE,
		NodeMask:       0,
	})
	if err != nil {
		return err
	}
	g.rtvDescriptorHeap = h
	defer func() {
		if ferr != nil {
			g.rtvDescriptorHeap.Release()
			g.rtvDescriptorHeap = nil
		}
	}()
	g.rtvDescriptorSize = g.device.GetDescriptorHandleIncrementSize(_D3D12_DESCRIPTOR_HEAP_TYPE_RTV)

	if err := g.pipelineStates.initialize(g.device); err != nil {
		return err
	}

	return nil
}

func createBuffer(device *iD3D12Device, bufferSize uint64, heapType _D3D12_HEAP_TYPE) (*iD3D12Resource1, error) {
	state := _D3D12_RESOURCE_STATE_GENERIC_READ
	if heapType == _D3D12_HEAP_TYPE_READBACK {
		state = _D3D12_RESOURCE_STATE_COPY_DEST
	}

	r, err := device.CreateCommittedResource(&_D3D12_HEAP_PROPERTIES{
		Type:                 heapType,
		CPUPageProperty:      _D3D12_CPU_PAGE_PROPERTY_UNKNOWN,
		MemoryPoolPreference: _D3D12_MEMORY_POOL_UNKNOWN,
		CreationNodeMask:     1,
		VisibleNodeMask:      1,
	}, _D3D12_HEAP_FLAG_NONE, &_D3D12_RESOURCE_DESC{
		Dimension:        _D3D12_RESOURCE_DIMENSION_BUFFER,
		Alignment:        0,
		Width:            bufferSize,
		Height:           1,
		DepthOrArraySize: 1,
		MipLevels:        1,
		Format:           _DXGI_FORMAT_UNKNOWN,
		SampleDesc: _DXGI_SAMPLE_DESC{
			Count:   1,
			Quality: 0,
		},
		Layout: _D3D12_TEXTURE_LAYOUT_ROW_MAJOR,
		Flags:  _D3D12_RESOURCE_FLAG_NONE,
	}, state, nil)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (g *Graphics) updateSwapChain(width, height int) error {
	if g.window == 0 {
		return errors.New("directx: the window handle is not initialized yet")
	}

	if g.swapChain == nil {
		if err := g.initSwapChain(width, height); err != nil {
			return err
		}
	} else {
		if err := g.resizeSwapChain(width, height); err != nil {
			return err
		}
	}

	return nil
}

func (g *Graphics) initSwapChain(width, height int) (ferr error) {
	// Create a swap chain.
	s, err := g.factory.CreateSwapChainForHwnd(unsafe.Pointer(g.commandQueue), g.window, &_DXGI_SWAP_CHAIN_DESC1{
		Width:       uint32(width),
		Height:      uint32(height),
		Format:      _DXGI_FORMAT_R8G8B8A8_UNORM,
		BufferUsage: _DXGI_USAGE_RENDER_TARGET_OUTPUT,
		BufferCount: frameCount,
		SwapEffect:  _DXGI_SWAP_EFFECT_FLIP_DISCARD,
		SampleDesc: _DXGI_SAMPLE_DESC{
			Count:   1,
			Quality: 0,
		},
	}, nil, nil)
	if err != nil {
		return err
	}
	s.As(&g.swapChain)
	defer func() {
		if ferr != nil {
			g.swapChain.Release()
			g.swapChain = nil
		}
	}()

	// TODO: Call factory.MakeWindowAssociation not to support fullscreen transitions?
	// TODO: Get the current buffer index?

	if err := g.createRenderTargetViews(); err != nil {
		return err
	}

	g.frameIndex = int(g.swapChain.GetCurrentBackBufferIndex())

	return nil
}

func (g *Graphics) resizeSwapChain(width, height int) error {
	if err := g.flushCommandList(g.copyCommandList); err != nil {
		return err
	}
	if err := g.copyCommandList.Close(); err != nil {
		return err
	}
	if err := g.flushCommandList(g.drawCommandList); err != nil {
		return err
	}
	if err := g.drawCommandList.Close(); err != nil {
		return err
	}

	for i := 0; i < frameCount; i++ {
		if err := g.waitForCommandQueue(i); err != nil {
			return err
		}
		if err := g.releaseResources(i); err != nil {
			return err
		}
	}

	for _, r := range g.renderTargets {
		r.Release()
	}

	if err := g.swapChain.ResizeBuffers(frameCount, uint32(width), uint32(height), _DXGI_FORMAT_R8G8B8A8_UNORM, 0); err != nil {
		return err
	}

	if err := g.createRenderTargetViews(); err != nil {
		return err
	}

	g.frameIndex = int(g.swapChain.GetCurrentBackBufferIndex())

	if err := g.drawCommandList.Reset(g.drawCommandAllocators[g.frameIndex], nil); err != nil {
		return err
	}
	if err := g.copyCommandList.Reset(g.copyCommandAllocators[g.frameIndex], nil); err != nil {
		return err
	}

	return nil
}

func (g *Graphics) createRenderTargetViews() (ferr error) {
	// Create frame resources.
	h := g.rtvDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
	for i := 0; i < frameCount; i++ {
		r, err := g.swapChain.GetBuffer(uint32(i))
		if err != nil {
			return err
		}
		g.renderTargets[i] = r
		defer func(i int) {
			if ferr != nil {
				g.renderTargets[i].Release()
				g.renderTargets[i] = nil
			}
		}(i)

		g.device.CreateRenderTargetView(r, nil, h)
		h.Offset(1, g.rtvDescriptorSize)
	}

	return nil
}

func (g *Graphics) SetWindow(window uintptr) {
	g.window = windows.HWND(window)
	// TODO: need to update the swap chain?
}

func (g *Graphics) Begin() error {
	g.frameIndex = 0
	// The swap chain is initialized when NewScreenFramebufferImage is called.
	// This must be called at the first frame.
	if g.swapChain != nil {
		g.frameIndex = int(g.swapChain.GetCurrentBackBufferIndex())
	}

	if err := g.drawCommandList.Reset(g.drawCommandAllocators[g.frameIndex], nil); err != nil {
		return err
	}

	if err := g.copyCommandList.Reset(g.copyCommandAllocators[g.frameIndex], nil); err != nil {
		return err
	}
	return nil
}

func (g *Graphics) End(present bool) error {
	// The swap chain might still be nil when Begin-End is invoked not by a frame (e.g., Image.At).

	// As copyCommandList and drawCommandList are exclusive, the order should not matter here.
	if err := g.flushCommandList(g.copyCommandList); err != nil {
		return err
	}
	if err := g.copyCommandList.Close(); err != nil {
		return err
	}

	if present {
		g.screenImage.transiteState(g.drawCommandList, _D3D12_RESOURCE_STATE_PRESENT)
	}

	if err := g.drawCommandList.Close(); err != nil {
		return err
	}
	g.commandQueue.ExecuteCommandLists([]*iD3D12GraphicsCommandList{g.drawCommandList})

	if present {
		if g.swapChain == nil {
			return fmt.Errorf("directx: the swap chain is not initialized yet at End")
		}

		var syncInterval uint32
		if g.vsyncEnabled {
			syncInterval = 1
		}
		if err := g.swapChain.Present(syncInterval, 0); err != nil {
			return err
		}

		// Wait for the previous frame.
		fence := g.fences[g.frameIndex]
		g.fenceValues[g.frameIndex]++
		if err := g.commandQueue.Signal(fence, g.fenceValues[g.frameIndex]); err != nil {
			return err
		}

		nextIndex := (g.frameIndex + 1) % frameCount
		if err := g.waitForCommandQueue(nextIndex); err != nil {
			return err
		}

		g.vertices[nextIndex] = g.vertices[nextIndex][:0]
		g.indices[nextIndex] = g.indices[nextIndex][:0]

		if err := g.releaseResources(nextIndex); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graphics) waitForCommandQueue(frameIndex int) error {
	expected := g.fenceValues[frameIndex]
	actual := g.fences[frameIndex].GetCompletedValue()
	if actual < expected {
		if err := g.fences[frameIndex].SetEventOnCompletion(expected, g.fenceWaitEvent); err != nil {
			return err
		}
		if _, err := windows.WaitForSingleObject(g.fenceWaitEvent, windows.INFINITE); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graphics) releaseResources(frameIndex int) error {
	for i, img := range g.disposedImages[frameIndex] {
		img.disposeImpl()
		g.disposedImages[frameIndex][i] = nil
	}
	g.disposedImages[frameIndex] = g.disposedImages[frameIndex][:0]

	for i, s := range g.disposedShaders[frameIndex] {
		s.disposeImpl()
		g.disposedShaders[frameIndex][i] = nil
	}
	g.disposedShaders[frameIndex] = g.disposedShaders[frameIndex][:0]

	if err := g.drawCommandAllocators[frameIndex].Reset(); err != nil {
		return err
	}
	if err := g.copyCommandAllocators[frameIndex].Reset(); err != nil {
		return err
	}

	return nil
}

// flushCommandList executes commands in the command list and waits for its completion.
//
// TODO: This is not efficient. Is it possible to make two command lists work in parallel?
func (g *Graphics) flushCommandList(commandList *iD3D12GraphicsCommandList) error {
	if err := commandList.Close(); err != nil {
		return err
	}

	f, err := g.device.CreateFence(0, _D3D12_FENCE_FLAG_NONE)
	if err != nil {
		return err
	}
	defer f.Release()

	g.commandQueue.ExecuteCommandLists([]*iD3D12GraphicsCommandList{commandList})

	const expected uint64 = 1
	g.commandQueue.Signal(f, expected)
	if f.GetCompletedValue() < expected {
		if err := f.SetEventOnCompletion(expected, g.fenceWaitEvent); err != nil {
			return err
		}
		if _, err := windows.WaitForSingleObject(g.fenceWaitEvent, windows.INFINITE); err != nil {
			return err
		}
	}

	switch commandList {
	case g.drawCommandList:
		if err := commandList.Reset(g.drawCommandAllocators[g.frameIndex], nil); err != nil {
			return err
		}
	case g.copyCommandList:
		if err := commandList.Reset(g.copyCommandAllocators[g.frameIndex], nil); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graphics) SetTransparent(transparent bool) {
}

func (g *Graphics) SetVertices(vertices []float32, indices []uint16) (ferr error) {
	// Create buffers if necessary.
	vidx := len(g.vertices[g.frameIndex])
	if cap(g.vertices[g.frameIndex]) > vidx {
		g.vertices[g.frameIndex] = g.vertices[g.frameIndex][:vidx+1]
	} else {
		g.vertices[g.frameIndex] = append(g.vertices[g.frameIndex], nil)
	}
	if g.vertices[g.frameIndex][vidx] == nil {
		// TODO: Use the default heap for efficienty. See the official example HelloTriangle.
		vs, err := createBuffer(g.device, graphics.IndicesNum*graphics.VertexFloatNum*uint64(unsafe.Sizeof(float32(0))), _D3D12_HEAP_TYPE_UPLOAD)
		if err != nil {
			return err
		}
		g.vertices[g.frameIndex][vidx] = vs
		defer func() {
			if ferr != nil {
				g.vertices[g.frameIndex][vidx].Release()
				g.vertices[g.frameIndex][vidx] = nil
			}
		}()
	}

	iidx := len(g.indices[g.frameIndex])
	if cap(g.indices[g.frameIndex]) > iidx {
		g.indices[g.frameIndex] = g.indices[g.frameIndex][:iidx+1]
	} else {
		g.indices[g.frameIndex] = append(g.indices[g.frameIndex], nil)
	}
	if g.indices[g.frameIndex][iidx] == nil {
		is, err := createBuffer(g.device, graphics.IndicesNum*uint64(unsafe.Sizeof(uint16(0))), _D3D12_HEAP_TYPE_UPLOAD)
		if err != nil {
			return err
		}
		g.indices[g.frameIndex][iidx] = is
		defer func() {
			if ferr != nil {
				g.indices[g.frameIndex][iidx].Release()
				g.indices[g.frameIndex][iidx] = nil
			}
		}()
	}

	r := _D3D12_RANGE{0, 0}

	m, err := g.vertices[g.frameIndex][vidx].Map(0, &r)
	if err != nil {
		return err
	}
	copyFloat32s(m, vertices)
	if err := g.vertices[g.frameIndex][vidx].Unmap(0, nil); err != nil {
		return err
	}

	m, err = g.indices[g.frameIndex][iidx].Map(0, &r)
	if err != nil {
		return err
	}
	copyUint16s(m, indices)
	if err := g.indices[g.frameIndex][iidx].Unmap(0, nil); err != nil {
		return err
	}

	return nil
}

func (g *Graphics) NewImage(width, height int) (graphicsdriver.Image, error) {
	desc := _D3D12_RESOURCE_DESC{
		Dimension:        _D3D12_RESOURCE_DIMENSION_TEXTURE2D,
		Alignment:        0,
		Width:            uint64(graphics.InternalImageSize(width)),
		Height:           uint32(graphics.InternalImageSize(height)),
		DepthOrArraySize: 1,
		MipLevels:        1,
		Format:           _DXGI_FORMAT_R8G8B8A8_UNORM,
		SampleDesc: _DXGI_SAMPLE_DESC{
			Count:   1,
			Quality: 0,
		},
		Layout: _D3D12_TEXTURE_LAYOUT_UNKNOWN,
		Flags:  _D3D12_RESOURCE_FLAG_ALLOW_RENDER_TARGET,
	}

	state := _D3D12_RESOURCE_STATE_PIXEL_SHADER_RESOURCE
	t, err := g.device.CreateCommittedResource(&_D3D12_HEAP_PROPERTIES{
		Type:                 _D3D12_HEAP_TYPE_DEFAULT, // Upload?
		CPUPageProperty:      _D3D12_CPU_PAGE_PROPERTY_UNKNOWN,
		MemoryPoolPreference: _D3D12_MEMORY_POOL_UNKNOWN,
		CreationNodeMask:     1,
		VisibleNodeMask:      1,
	}, _D3D12_HEAP_FLAG_NONE, &desc, state, nil)
	if err != nil {
		return nil, err
	}

	layouts, numRows, rowSizeInBytes, totalBytes := g.device.GetCopyableFootprints(&desc, 0, 1, 0)

	i := &Image{
		graphics:       g,
		id:             g.genNextImageID(),
		width:          width,
		height:         height,
		texture:        t,
		state:          state,
		layouts:        layouts,
		numRows:        numRows,
		rowSizeInBytes: rowSizeInBytes,
		totalBytes:     totalBytes,
	}
	g.addImage(i)
	return i, nil
}

func (g *Graphics) NewScreenFramebufferImage(width, height int) (graphicsdriver.Image, error) {
	if err := g.updateSwapChain(width, height); err != nil {
		return nil, err
	}

	i := &Image{
		graphics: g,
		id:       g.genNextImageID(),
		width:    width,
		height:   height,
		screen:   true,
		state:    _D3D12_RESOURCE_STATE_PRESENT,
	}
	g.addImage(i)
	return i, nil
}

func (g *Graphics) addImage(img *Image) {
	if g.images == nil {
		g.images = map[graphicsdriver.ImageID]*Image{}
	}
	if _, ok := g.images[img.id]; ok {
		panic(fmt.Sprintf("directx: image ID %d was already registered", img.id))
	}
	g.images[img.id] = img
	if img.screen {
		g.screenImage = img
	}
}

func (g *Graphics) removeImage(img *Image) {
	delete(g.images, img.id)
	g.disposedImages[g.frameIndex] = append(g.disposedImages[g.frameIndex], img)
	if img.screen {
		g.screenImage = nil
	}
}

func (g *Graphics) addShader(s *Shader) {
	if g.shaders == nil {
		g.shaders = map[graphicsdriver.ShaderID]*Shader{}
	}
	if _, ok := g.shaders[s.id]; ok {
		panic(fmt.Sprintf("directx: shader ID %d was already registered", s.id))
	}
	g.shaders[s.id] = s
}

func (g *Graphics) removeShader(s *Shader) {
	delete(g.shaders, s.id)
	g.disposedShaders[g.frameIndex] = append(g.disposedShaders[g.frameIndex], s)
}

func (g *Graphics) SetVsyncEnabled(enabled bool) {
	g.vsyncEnabled = enabled
}

func (g *Graphics) SetFullscreen(fullscreen bool) {
}

func (g *Graphics) FramebufferYDirection() graphicsdriver.YDirection {
	return graphicsdriver.Downward
}

func (g *Graphics) NDCYDirection() graphicsdriver.YDirection {
	return graphicsdriver.Upward
}

func (g *Graphics) NeedsRestoring() bool {
	return false
}

func (g *Graphics) NeedsClearingScreen() bool {
	// TODO: Confirm this is really true.
	return true
}

func (g *Graphics) IsGL() bool {
	return false
}

func (g *Graphics) IsDirectX() bool {
	return true
}

func (g *Graphics) HasHighPrecisionFloat() bool {
	return true
}

func (g *Graphics) MaxImageSize() int {
	return _D3D12_REQ_TEXTURE2D_U_OR_V_DIMENSION
}

func (g *Graphics) NewShader(program *shaderir.Program) (graphicsdriver.Shader, error) {
	src := hlsl.Compile(program)
	for i, l := range strings.Split(src, "\n") {
		fmt.Printf("%d: %s\n", i+1, l)
	}
	vsh, psh, err := newShader([]byte(src), nil)
	if err != nil {
		return nil, err
	}

	s := &Shader{
		graphics:     g,
		id:           g.genNextShaderID(),
		vertexShader: vsh,
		pixelShader:  psh,
	}
	g.addShader(s)
	return s, nil
}

func (g *Graphics) DrawTriangles(dstID graphicsdriver.ImageID, srcs [graphics.ShaderImageNum]graphicsdriver.ImageID, offsets [graphics.ShaderImageNum - 1][2]float32, shaderID graphicsdriver.ShaderID, indexLen int, indexOffset int, mode graphicsdriver.CompositeMode, colorM graphicsdriver.ColorM, filter graphicsdriver.Filter, address graphicsdriver.Address, dstRegion, srcRegion graphicsdriver.Region, uniforms []graphicsdriver.Uniform, evenOdd bool) error {
	if err := g.flushCommandList(g.copyCommandList); err != nil {
		return err
	}

	dst := g.images[dstID]

	var shader *Shader
	if shaderID != graphicsdriver.InvalidShaderID {
		shader = g.shaders[shaderID]
	}

	if err := dst.setAsRenderTarget(g.device); err != nil {
		return err
	}

	var srcImages [graphics.ShaderImageNum]*Image
	for i, srcID := range srcs {
		src := g.images[srcID]
		if src == nil {
			continue
		}
		srcImages[i] = src
		src.transiteState(g.drawCommandList, _D3D12_RESOURCE_STATE_PIXEL_SHADER_RESOURCE)
	}
	if shader == nil {
		key := builtinPipelineStatesKey{
			useColorM:     !colorM.IsIdentity(),
			compositeMode: mode,
			filter:        filter,
			address:       address,
			screen:        dst.screen,
		}
		if err := g.pipelineStates.useBuiltinGraphicsPipelineState(g.device, g.drawCommandList, g.frameIndex, key, dst, srcImages, srcRegion, colorM); err != nil {
			return err
		}
	} else {
		state, err := shader.pipelineState(mode)
		if err != nil {
			return err
		}
		if err := g.pipelineStates.useGraphicsPipelineState(g.device, g.drawCommandList, g.frameIndex, state, dst, srcImages, srcRegion); err != nil {
			return err
		}
	}

	w, h := dst.internalSize()
	g.drawCommandList.RSSetViewports(1, &_D3D12_VIEWPORT{
		TopLeftX: 0,
		TopLeftY: 0,
		Width:    float32(w),
		Height:   float32(h),
		MinDepth: _D3D12_MIN_DEPTH,
		MaxDepth: _D3D12_MAX_DEPTH,
	})
	g.drawCommandList.RSSetScissorRects(1, &_D3D12_RECT{
		left:   int32(dstRegion.X),
		top:    int32(dstRegion.Y),
		right:  int32(dstRegion.X + dstRegion.Width),
		bottom: int32(dstRegion.Y + dstRegion.Height),
	})

	g.drawCommandList.IASetPrimitiveTopology(_D3D_PRIMITIVE_TOPOLOGY_TRIANGLELIST)
	g.drawCommandList.IASetVertexBuffers(0, 1, &_D3D12_VERTEX_BUFFER_VIEW{
		BufferLocation: g.vertices[g.frameIndex][len(g.vertices[g.frameIndex])-1].GetGPUVirtualAddress(),
		SizeInBytes:    graphics.IndicesNum * graphics.VertexFloatNum * uint32(unsafe.Sizeof(float32(0))),
		StrideInBytes:  graphics.VertexFloatNum * uint32(unsafe.Sizeof(float32(0))),
	})
	g.drawCommandList.IASetIndexBuffer(&_D3D12_INDEX_BUFFER_VIEW{
		BufferLocation: g.indices[g.frameIndex][len(g.indices[g.frameIndex])-1].GetGPUVirtualAddress(),
		SizeInBytes:    graphics.IndicesNum * uint32(unsafe.Sizeof(uint16(0))),
		Format:         _DXGI_FORMAT_R16_UINT,
	})

	g.drawCommandList.DrawIndexedInstanced(uint32(indexLen), 1, uint32(indexOffset), 0, 0)

	return nil
}

func (g *Graphics) genNextImageID() graphicsdriver.ImageID {
	g.nextImageID++
	return g.nextImageID
}

func (g *Graphics) genNextShaderID() graphicsdriver.ShaderID {
	g.nextShaderID++
	return g.nextShaderID
}

type Image struct {
	graphics *Graphics
	id       graphicsdriver.ImageID
	width    int
	height   int
	screen   bool

	state                  _D3D12_RESOURCE_STATES
	texture                *iD3D12Resource1
	layouts                _D3D12_PLACED_SUBRESOURCE_FOOTPRINT
	numRows                uint
	rowSizeInBytes         uint64
	totalBytes             uint64
	uploadingStagingBuffer *iD3D12Resource1
	readingStagingBuffer   *iD3D12Resource1
	rtvDescriptorHeap      *iD3D12DescriptorHeap
}

func (i *Image) ID() graphicsdriver.ImageID {
	return i.id
}

func (i *Image) Dispose() {
	// Dipose the images later as this image might still be used.
	i.graphics.removeImage(i)
}

func (i *Image) disposeImpl() {
	if i.rtvDescriptorHeap != nil {
		i.rtvDescriptorHeap.Release()
		i.rtvDescriptorHeap = nil
	}
	if i.uploadingStagingBuffer != nil {
		i.uploadingStagingBuffer.Release()
		i.uploadingStagingBuffer = nil
	}
	if i.readingStagingBuffer != nil {
		i.readingStagingBuffer.Release()
		i.readingStagingBuffer = nil
	}
	if i.texture != nil {
		i.texture.Release()
		i.texture = nil
	}
}

func (*Image) IsInvalidated() bool {
	return false
}

func (i *Image) ensureUploadingStagingBuffer() error {
	if i.uploadingStagingBuffer != nil {
		return nil
	}
	var err error
	i.uploadingStagingBuffer, err = createBuffer(i.graphics.device, i.totalBytes, _D3D12_HEAP_TYPE_UPLOAD)
	if err != nil {
		return err
	}
	return nil
}

func (i *Image) ensureReadingStagingBuffer() error {
	if i.readingStagingBuffer != nil {
		return nil
	}
	var err error
	i.readingStagingBuffer, err = createBuffer(i.graphics.device, i.totalBytes, _D3D12_HEAP_TYPE_READBACK)
	if err != nil {
		return err
	}
	return nil
}

func (i *Image) ReadPixels(buf []byte) error {
	if i.screen {
		return errors.New("directx: Pixels cannot be called on the screen")
	}

	if err := i.graphics.flushCommandList(i.graphics.drawCommandList); err != nil {
		return err
	}

	if err := i.ensureReadingStagingBuffer(); err != nil {
		return err
	}

	i.transiteState(i.graphics.copyCommandList, _D3D12_RESOURCE_STATE_COPY_SOURCE)

	r := _D3D12_RANGE{0, 0}
	m, err := i.readingStagingBuffer.Map(0, &r)
	if err != nil {
		return err
	}

	var dstBytes []byte
	h := (*reflect.SliceHeader)(unsafe.Pointer(&dstBytes))
	h.Data = uintptr(m)
	h.Len = int(i.totalBytes)
	h.Cap = int(i.totalBytes)

	dst := _D3D12_TEXTURE_COPY_LOCATION_PlacedFootPrint{
		pResource:       i.readingStagingBuffer,
		Type:            _D3D12_TEXTURE_COPY_TYPE_PLACED_FOOTPRINT,
		PlacedFootprint: i.layouts,
	}
	src := _D3D12_TEXTURE_COPY_LOCATION_SubresourceIndex{
		pResource:        i.texture,
		Type:             _D3D12_TEXTURE_COPY_TYPE_SUBRESOURCE_INDEX,
		SubresourceIndex: 0,
	}
	i.graphics.copyCommandList.CopyTextureRegion_PlacedFootPrint_SubresourceIndex(
		&dst, 0, 0, 0, &src, &_D3D12_BOX{
			left:   0,
			top:    0,
			front:  0,
			right:  uint32(i.width),
			bottom: uint32(i.height),
			back:   1,
		})

	if err := i.graphics.flushCommandList(i.graphics.copyCommandList); err != nil {
		return err
	}

	copy(buf, dstBytes)

	if err := i.readingStagingBuffer.Unmap(0, nil); err != nil {
		return err
	}

	return nil
}

func (i *Image) ReplacePixels(args []*graphicsdriver.ReplacePixelsArgs) error {
	if i.screen {
		return errors.New("directx: ReplacePixels cannot be called on the screen")
	}

	if err := i.graphics.flushCommandList(i.graphics.drawCommandList); err != nil {
		return err
	}

	if err := i.ensureUploadingStagingBuffer(); err != nil {
		return err
	}

	i.transiteState(i.graphics.copyCommandList, _D3D12_RESOURCE_STATE_COPY_DEST)

	r := _D3D12_RANGE{0, 0}
	m, err := i.uploadingStagingBuffer.Map(0, &r)
	if err != nil {
		return err
	}

	var srcBytes []byte
	h := (*reflect.SliceHeader)(unsafe.Pointer(&srcBytes))
	h.Data = uintptr(m)
	h.Len = int(i.totalBytes)
	h.Cap = int(i.totalBytes)
	for _, a := range args {
		for j := 0; j < a.Height; j++ {
			copy(srcBytes[(a.Y+j)*int(i.rowSizeInBytes)+a.X*4:], a.Pixels[j*a.Width*4:(j+1)*a.Width*4])
		}

		dst := _D3D12_TEXTURE_COPY_LOCATION_SubresourceIndex{
			pResource:        i.texture,
			Type:             _D3D12_TEXTURE_COPY_TYPE_SUBRESOURCE_INDEX,
			SubresourceIndex: 0,
		}
		src := _D3D12_TEXTURE_COPY_LOCATION_PlacedFootPrint{
			pResource:       i.uploadingStagingBuffer,
			Type:            _D3D12_TEXTURE_COPY_TYPE_PLACED_FOOTPRINT,
			PlacedFootprint: i.layouts,
		}
		i.graphics.copyCommandList.CopyTextureRegion_SubresourceIndex_PlacedFootPrint(
			&dst, uint32(a.X), uint32(a.Y), 0, &src, &_D3D12_BOX{
				left:   uint32(a.X),
				top:    uint32(a.Y),
				front:  0,
				right:  uint32(a.X + a.Width),
				bottom: uint32(a.Y + a.Height),
				back:   1,
			})
	}

	if err := i.uploadingStagingBuffer.Unmap(0, nil); err != nil {
		return err
	}

	return nil
}

func (i *Image) resource() *iD3D12Resource1 {
	if i.screen {
		return i.graphics.renderTargets[i.graphics.frameIndex]
	}
	return i.texture
}

func (i *Image) transiteState(commandList *iD3D12GraphicsCommandList, newState _D3D12_RESOURCE_STATES) {
	if i.state == newState {
		return
	}

	commandList.ResourceBarrier(1, &_D3D12_RESOURCE_BARRIER_Transition{
		Type:  _D3D12_RESOURCE_BARRIER_TYPE_TRANSITION,
		Flags: _D3D12_RESOURCE_BARRIER_FLAG_NONE,
		Transition: _D3D12_RESOURCE_TRANSITION_BARRIER{
			pResource:   i.resource(),
			Subresource: _D3D12_RESOURCE_BARRIER_ALL_SUBRESOURCES,
			StateBefore: i.state,
			StateAfter:  newState,
		},
	})
	i.state = newState
}

func (i *Image) internalSize() (int, int) {
	if i.screen {
		return i.width, i.height
	}
	return graphics.InternalImageSize(i.width), graphics.InternalImageSize(i.height)
}

func (i *Image) setAsRenderTarget(device *iD3D12Device) error {
	i.transiteState(i.graphics.drawCommandList, _D3D12_RESOURCE_STATE_RENDER_TARGET)

	if err := i.ensureRenderTargetView(device); err != nil {
		return err
	}

	if i.screen {
		rtv := i.graphics.rtvDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
		rtv.Offset(int32(i.graphics.frameIndex), i.graphics.rtvDescriptorSize)
		i.graphics.drawCommandList.OMSetRenderTargets(1, &rtv, false, nil)
		return nil
	}

	rtv := i.rtvDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
	i.graphics.drawCommandList.OMSetRenderTargets(1, &rtv, false, nil)

	return nil
}

func (i *Image) ensureRenderTargetView(device *iD3D12Device) error {
	if i.screen {
		return nil
	}

	if i.rtvDescriptorHeap != nil {
		return nil
	}

	h, err := device.CreateDescriptorHeap(&_D3D12_DESCRIPTOR_HEAP_DESC{
		Type:           _D3D12_DESCRIPTOR_HEAP_TYPE_RTV,
		NumDescriptors: 1,
		Flags:          _D3D12_DESCRIPTOR_HEAP_FLAG_NONE,
		NodeMask:       0,
	})
	if err != nil {
		return err
	}
	i.rtvDescriptorHeap = h

	rtv := i.rtvDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
	device.CreateRenderTargetView(i.texture, nil, rtv)

	return nil
}

func copyFloat32s(dst unsafe.Pointer, src []float32) {
	var dsts []float32
	h := (*reflect.SliceHeader)(unsafe.Pointer(&dsts))
	h.Data = uintptr(dst)
	h.Len = len(src)
	h.Cap = len(src)
	copy(dsts, src)
}

func copyUint16s(dst unsafe.Pointer, src []uint16) {
	var dsts []uint16
	h := (*reflect.SliceHeader)(unsafe.Pointer(&dsts))
	h.Data = uintptr(dst)
	h.Len = len(src)
	h.Cap = len(src)
	copy(dsts, src)
}

type Shader struct {
	graphics       *Graphics
	id             graphicsdriver.ShaderID
	vertexShader   *iD3DBlob
	pixelShader    *iD3DBlob
	pipelineStates map[graphicsdriver.CompositeMode]*iD3D12PipelineState
}

func (s *Shader) ID() graphicsdriver.ShaderID {
	return s.id
}

func (s *Shader) Dispose() {
	s.graphics.removeShader(s)
}

func (s *Shader) disposeImpl() {
	for c, p := range s.pipelineStates {
		p.Release()
		delete(s.pipelineStates, c)
	}

	if s.pixelShader != nil {
		s.pixelShader.Release()
		s.pixelShader = nil
	}
	if s.vertexShader != nil {
		s.vertexShader.Release()
		s.vertexShader = nil
	}
}

func (s *Shader) pipelineState(compositeMode graphicsdriver.CompositeMode) (*iD3D12PipelineState, error) {
	if state, ok := s.pipelineStates[compositeMode]; ok {
		return state, nil
	}

	state, err := s.graphics.pipelineStates.newPipelineState(s.graphics.device, s.vertexShader, s.pixelShader, compositeMode)
	if err != nil {
		return nil, err
	}
	if s.pipelineStates == nil {
		s.pipelineStates = map[graphicsdriver.CompositeMode]*iD3D12PipelineState{}
	}
	s.pipelineStates[compositeMode] = state
	return state, nil
}
