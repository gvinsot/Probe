// Package icon draws the application icon at run time: the website logo, the
// black Probe "P", on a white rounded tile, with an orange dot when documents
// wait for a review. The logo is the logo.jpeg the interface already embeds,
// so the icon needs no binary asset per platform and size.
package icon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io/fs"
	"math"
	"sync"

	"github.com/gvinsot/Probe/desktop/web"
)

var (
	ink    = color.RGBA{0x00, 0x00, 0x00, 0xff}
	paper  = color.RGBA{0xff, 0xff, 0xff, 0xff}
	orange = color.RGBA{0xd9, 0x60, 0x3b, 0xff}
)

// PNG returns the icon at the given size.
func PNG(size int, alert bool) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, draw(size, alert))
	return buf.Bytes()
}

// ICO returns a Windows icon holding PNG images at the usual tray sizes.
func ICO(alert bool) []byte {
	sizes := []int{16, 20, 24, 32, 40, 48, 64}
	images := make([][]byte, len(sizes))
	for i, s := range sizes {
		images[i] = PNG(s, alert)
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := byte(s)
		if s >= 256 {
			dim = 0
		}
		buf.Write([]byte{dim, dim, 0, 0})
		binary.Write(&buf, binary.LittleEndian, [2]uint16{1, 32})
		binary.Write(&buf, binary.LittleEndian, [2]uint32{uint32(len(images[i])), uint32(offset)})
		offset += len(images[i])
	}
	for _, img := range images {
		buf.Write(img)
	}
	return buf.Bytes()
}

// The logo sits in a 32-unit design: a 32x32 tile with 8-unit corners and the
// "P" fitted in a centered box of logoBox units.
const logoBox = 22

// mask is the ink coverage of the logo, 0 (paper) to 1 (ink), cropped to the
// "P" itself.
type mask struct {
	w, h int
	v    []float64
}

var loadLogo = sync.OnceValue(func() *mask {
	data, err := fs.ReadFile(web.Assets, "public/logo.jpeg")
	if err != nil {
		return nil
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	b := img.Bounds()
	// The JPEG has no transparency: darkness is ink.
	full := make([]float64, b.Dx()*b.Dy())
	minX, minY, maxX, maxY := b.Dx(), b.Dy(), -1, -1
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			g := color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray)
			// Map the compression noise near white and black to exact values.
			v := math.Min(1, math.Max(0, (235-float64(g.Y))/215))
			full[y*b.Dx()+x] = v
			if v > 0.5 {
				minX, minY = min(minX, x), min(minY, y)
				maxX, maxY = max(maxX, x), max(maxY, y)
			}
		}
	}
	if maxX < 0 {
		return nil
	}
	m := &mask{w: maxX - minX + 1, h: maxY - minY + 1}
	m.v = make([]float64, m.w*m.h)
	for y := 0; y < m.h; y++ {
		copy(m.v[y*m.w:(y+1)*m.w], full[(minY+y)*b.Dx()+minX:])
	}
	return m
})

// coverage returns the logo ink over each pixel of a size x size icon, by
// sampling it on a grid at least as fine as the source pixels.
func coverage(size int) []float64 {
	out := make([]float64, size*size)
	m := loadLogo()
	if m == nil {
		return out
	}
	scale := float64(size) / 32
	// Output pixels per source pixel, the logo fitted in logoBox units.
	k := logoBox * scale / float64(max(m.w, m.h))
	ox := (float64(size) - float64(m.w)*k) / 2
	oy := (float64(size) - float64(m.h)*k) / 2
	ss := int(math.Ceil(1 / k))
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var sum float64
			for sy := 0; sy < ss; sy++ {
				y := int(math.Floor((float64(py) + (float64(sy)+0.5)/float64(ss) - oy) / k))
				if y < 0 || y >= m.h {
					continue
				}
				for sx := 0; sx < ss; sx++ {
					x := int(math.Floor((float64(px) + (float64(sx)+0.5)/float64(ss) - ox) / k))
					if x >= 0 && x < m.w {
						sum += m.v[y*m.w+x]
					}
				}
			}
			out[py*size+px] = sum / float64(ss*ss)
		}
	}
	return out
}

// draw renders the icon, the tile edges and the badge supersampled 4x per
// pixel.
func draw(size int, alert bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	cover := coverage(size)
	const ss = 4
	scale := float64(size) / 32
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			tile := mix(paper, ink, cover[py*size+px])
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / scale
					y := (float64(py) + (float64(sy)+0.5)/ss) / scale
					c, ok := sample(x, y, alert, tile)
					if !ok {
						continue
					}
					r += float64(c.R)
					g += float64(c.G)
					b += float64(c.B)
					a += 255
				}
			}
			n := float64(ss * ss)
			if a == 0 {
				continue
			}
			// Premultiplied: color sums already weight by coverage.
			img.SetRGBA(px, py, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), uint8(a / n)})
		}
	}
	return img
}

// mix blends from a to b by t in [0, 1].
func mix(a, b color.RGBA, t float64) color.RGBA {
	f := func(u, v uint8) uint8 { return uint8(math.Round(float64(u)*(1-t) + float64(v)*t)) }
	return color.RGBA{f(a.R, b.R), f(a.G, b.G), f(a.B, b.B), 0xff}
}

// sample returns the color at a point of the 32x32 design, tile being the
// tile color with the logo over the current pixel.
func sample(x, y float64, alert bool, tile color.RGBA) (color.RGBA, bool) {
	if alert {
		// Badge in the top right corner, ringed with white.
		d := math.Hypot(x-25, y-7)
		if d <= 6.2 {
			return orange, true
		}
		if d <= 8 {
			return paper, true
		}
	}
	if !inRoundedRect(x, y, 0, 0, 32, 32, 8) {
		return color.RGBA{}, false
	}
	return tile, true
}

func inRoundedRect(x, y, x0, y0, w, h, rad float64) bool {
	if x < x0 || y < y0 || x > x0+w || y > y0+h {
		return false
	}
	cx := math.Min(math.Max(x, x0+rad), x0+w-rad)
	cy := math.Min(math.Max(y, y0+rad), y0+h-rad)
	return math.Hypot(x-cx, y-cy) <= rad
}
