// Copyright 2019 The Ebiten Authors
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

package buffered

import (
	"fmt"
	"image"
	"image/color"

	"github.com/hajimehoshi/ebiten/internal/affine"
	"github.com/hajimehoshi/ebiten/internal/driver"
	"github.com/hajimehoshi/ebiten/internal/mipmap"
	"github.com/hajimehoshi/ebiten/internal/shaderir"
)

type Image struct {
	img    *mipmap.Mipmap
	width  int
	height int

	hasFill   bool
	fillColor color.RGBA

	pixels               []byte
	needsToResolvePixels bool
}

func BeginFrame() error {
	if err := mipmap.BeginFrame(); err != nil {
		return err
	}
	return flushDelayedCommands()
}

func EndFrame() error {
	return mipmap.EndFrame()
}

func NewImage(width, height int, volatile bool) *Image {
	i := &Image{}
	i.initialize(width, height, volatile)
	return i
}

func (i *Image) initialize(width, height int, volatile bool) {
	if tryAddDelayedCommand(func(obj interface{}) error {
		i.initialize(width, height, volatile)
		return nil
	}, nil) {
		return
	}
	i.img = mipmap.New(width, height, volatile)
	i.width = width
	i.height = height
}

func NewScreenFramebufferImage(width, height int) *Image {
	i := &Image{}
	i.initializeAsScreenFramebuffer(width, height)
	return i
}

func (i *Image) initializeAsScreenFramebuffer(width, height int) {
	if tryAddDelayedCommand(func(obj interface{}) error {
		i.initializeAsScreenFramebuffer(width, height)
		return nil
	}, nil) {
		return
	}

	i.img = mipmap.NewScreenFramebufferMipmap(width, height)
	i.width = width
	i.height = height
}

func (i *Image) invalidatePendingPixels() {
	i.pixels = nil
	i.needsToResolvePixels = false
	i.hasFill = false
}

func (i *Image) resolvePendingPixels(keepPendingPixels bool) {
	if i.needsToResolvePixels && i.hasFill {
		panic("buffered: needsToResolvePixels and hasFill must not be true at the same time")
	}
	if i.needsToResolvePixels {
		i.img.ReplacePixels(i.pixels)
		if !keepPendingPixels {
			i.pixels = nil
		}
		i.needsToResolvePixels = false
	}
	i.resolvePendingFill()
}

func (i *Image) resolvePendingFill() {
	if !i.hasFill {
		return
	}
	i.img.Fill(i.fillColor)
	i.hasFill = false
}

func (i *Image) MarkDisposed() {
	if tryAddDelayedCommand(func(obj interface{}) error {
		i.MarkDisposed()
		return nil
	}, nil) {
		return
	}
	i.invalidatePendingPixels()
	i.img.MarkDisposed()
}

func (img *Image) Pixels(x, y, width, height int) (pix []byte, err error) {
	checkDelayedCommandsFlushed("Pixels")

	if !image.Rect(x, y, x+width, y+height).In(image.Rect(0, 0, img.width, img.height)) {
		return nil, fmt.Errorf("buffered: out of range")
	}

	pix = make([]byte, 4*width*height)

	// If there are pixels or pending fillling that needs to be resolved, use this rather than resolving.
	// Resolving them needs to access GPU and is expensive (#1137).
	if img.hasFill {
		for i := 0; i < len(pix)/4; i++ {
			pix[4*i] = img.fillColor.R
			pix[4*i+1] = img.fillColor.G
			pix[4*i+2] = img.fillColor.B
			pix[4*i+3] = img.fillColor.A
		}
		return pix, nil
	}

	if img.pixels == nil {
		pix, err := img.img.Pixels(0, 0, img.width, img.height)
		if err != nil {
			return nil, err
		}
		img.pixels = pix
	}

	for j := 0; j < height; j++ {
		copy(pix[4*j*width:4*(j+1)*width], img.pixels[4*((j+y)*img.width+x):])
	}
	return pix, nil
}

func (img *Image) Convert2RGBA() *image.RGBA{
	var pix []byte
	if img.hasFill {
		pix = make([]byte, 4*img.width*img.height)
		for i := 0; i < len(pix)/4; i++ {
			pix[4*i] = img.fillColor.R
			pix[4*i+1] = img.fillColor.G
			pix[4*i+2] = img.fillColor.B
			pix[4*i+3] = img.fillColor.A
		}
	}else{
		pix = img.pixels
	}

	return &image.RGBA{
		Pix:    pix,
		Stride: 4 * img.width,
		Rect:   image.Rectangle{
			Min: image.Point{
				X: 0,
				Y: 0,
			},
			Max: image.Point{
				X: img.width,
				Y: img.height,
			},
		},
	}
}

func (i *Image) Dump(name string, blackbg bool) error {
	checkDelayedCommandsFlushed("Dump")
	return i.img.Dump(name, blackbg)
}

func (i *Image) Fill(clr color.RGBA) {
	if tryAddDelayedCommand(func(obj interface{}) error {
		i.Fill(clr)
		return nil
	}, nil) {
		return
	}

	// Defer filling the image so that successive fillings will be merged into one (#1134).
	i.invalidatePendingPixels()
	i.fillColor = clr
	i.hasFill = true
}

func (i *Image) ReplacePixels(pix []byte, x, y, width, height int) error {
	if l := 4 * width * height; len(pix) != l {
		panic(fmt.Sprintf("buffered: len(pix) was %d but must be %d", len(pix), l))
	}

	if tryAddDelayedCommand(func(copied interface{}) error {
		i.ReplacePixels(copied.([]byte), x, y, width, height)
		return nil
	}, func() interface{} {
		copied := make([]byte, len(pix))
		copy(copied, pix)
		return copied
	}) {
		return nil
	}

	if x == 0 && y == 0 && width == i.width && height == i.height {
		i.invalidatePendingPixels()

		// Don't call (*mipmap.Mipmap).ReplacePixels here. Let's defer it to reduce GPU operations as much as
		// posssible. This is a necessary optimization for sub-images: as sub-images are actually used and,
		// have to allocate their region on a texture atlas, while their original image doesn't have to
		// allocate its region on a texture atlas (#896).
		copied := make([]byte, len(pix))
		copy(copied, pix)
		i.pixels = copied
		i.needsToResolvePixels = true
		return nil
	}

	i.resolvePendingFill()

	// TODO: Can we use (*restorable.Image).ReplacePixels?
	if i.pixels == nil {
		pix, err := i.img.Pixels(0, 0, i.width, i.height)
		if err != nil {
			return err
		}
		i.pixels = pix
	}
	i.replacePendingPixels(pix, x, y, width, height)
	return nil
}

func (i *Image) replacePendingPixels(pix []byte, x, y, width, height int) {
	for j := 0; j < height; j++ {
		copy(i.pixels[4*((j+y)*i.width+x):], pix[4*j*width:4*(j+1)*width])
	}
	i.needsToResolvePixels = true
}

func (i *Image) CopyPixels(img *Image, x, y, width, height int) error {
	if tryAddDelayedCommand(func(obj interface{}) error {
		i.CopyPixels(img, x, y, width, height)
		return nil
	}, nil) {
		return nil
	}

	pix, err := img.Pixels(x, y, width, height)
	if err != nil {
		return err
	}
	if err := i.ReplacePixels(pix, 0, 0, width, height); err != nil {
		return err
	}
	return nil
}

func (i *Image) DrawImage(src *Image, bounds image.Rectangle, a, b, c, d, tx, ty float32, colorm *affine.ColorM, mode driver.CompositeMode, filter driver.Filter) {
	if i == src {
		panic("buffered: Image.DrawImage: src must be different from the receiver")
	}

	g := mipmap.GeoM{
		A:  a,
		B:  b,
		C:  c,
		D:  d,
		Tx: tx,
		Ty: ty,
	}

	if tryAddDelayedCommand(func(obj interface{}) error {
		i.drawImage(src, bounds, g, colorm, mode, filter)
		return nil
	}, nil) {
		return
	}

	i.drawImage(src, bounds, g, colorm, mode, filter)
}

func (i *Image) drawImage(src *Image, bounds image.Rectangle, g mipmap.GeoM, colorm *affine.ColorM, mode driver.CompositeMode, filter driver.Filter) {
	src.resolvePendingPixels(true)
	i.resolvePendingPixels(false)
	i.img.DrawImage(src.img, bounds, g, colorm, mode, filter)
}

// DrawTriangles draws the src image with the given vertices.
//
// Copying vertices and indices is the caller's responsibility.
func (i *Image) DrawTriangles(src *Image, vertices []float32, indices []uint16, colorm *affine.ColorM, mode driver.CompositeMode, filter driver.Filter, address driver.Address, shader *Shader, uniforms []interface{}) {
	var srcs []*Image
	if src != nil {
		srcs = append(srcs, src)
	}
	for _, u := range uniforms {
		if src, ok := u.(*Image); ok {
			srcs = append(srcs, src)
		}
	}

	for _, src := range srcs {
		if i == src {
			panic("buffered: Image.DrawTriangles: src must be different from the receiver")
		}
	}

	if tryAddDelayedCommand(func(obj interface{}) error {
		// Arguments are not copied. Copying is the caller's responsibility.
		i.DrawTriangles(src, vertices, indices, colorm, mode, filter, address, shader, uniforms)
		return nil
	}, nil) {
		return
	}

	for _, src := range srcs {
		src.resolvePendingPixels(true)
	}
	i.resolvePendingPixels(false)

	var s *mipmap.Shader
	if shader != nil {
		s = shader.shader
	}
	us := make([]interface{}, len(uniforms))
	for k, v := range uniforms {
		switch v := v.(type) {
		case *Image:
			i.resolvePendingPixels(true)
			us[k] = v.img
		default:
			us[k] = v
		}
	}

	var srcImg *mipmap.Mipmap
	if src != nil {
		srcImg = src.img
	}
	i.img.DrawTriangles(srcImg, vertices, indices, colorm, mode, filter, address, s, us)
}

type Shader struct {
	shader *mipmap.Shader
}

func NewShader(program *shaderir.Program) *Shader {
	return &Shader{
		shader: mipmap.NewShader(program),
	}
}

func (s *Shader) MarkDisposed() {
	s.shader.MarkDisposed()
	s.shader = nil
}
