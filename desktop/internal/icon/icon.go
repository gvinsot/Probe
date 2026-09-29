// Package icon draws the application icon at run time: the Probe check mark
// on the brand teal, with an orange dot when documents wait for a review.
// Drawing it avoids shipping binary assets per platform and size.
package icon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

var (
	teal   = color.RGBA{0x0f, 0x7b, 0x6c, 0xff}
	orange = color.RGBA{0xd9, 0x60, 0x3b, 0xff}
	white  = color.RGBA{0xff, 0xff, 0xff, 0xff}
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

// draw renders the 32-unit design of favicon.svg, supersampled 4x per pixel.
func draw(size int, alert bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	scale := float64(size) / 32
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / scale
					y := (float64(py) + (float64(sy)+0.5)/ss) / scale
					c, ok := sample(x, y, alert)
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

// sample returns the color at a point of the 32x32 design.
func sample(x, y float64, alert bool) (color.RGBA, bool) {
	if alert {
		// Badge in the top right corner, ringed with white.
		d := math.Hypot(x-25, y-7)
		if d <= 6.2 {
			return orange, true
		}
		if d <= 8 {
			return white, true
		}
	}
	if !inRoundedRect(x, y, 0, 0, 32, 32, 8) {
		return color.RGBA{}, false
	}
	// Check mark: M9 16.5 L13.5 21 L23 11.5, stroke width 3, round caps.
	if segDist(x, y, 9, 16.5, 13.5, 21) <= 1.5 || segDist(x, y, 13.5, 21, 23, 11.5) <= 1.5 {
		return white, true
	}
	return teal, true
}

func inRoundedRect(x, y, x0, y0, w, h, rad float64) bool {
	if x < x0 || y < y0 || x > x0+w || y > y0+h {
		return false
	}
	cx := math.Min(math.Max(x, x0+rad), x0+w-rad)
	cy := math.Min(math.Max(y, y0+rad), y0+h-rad)
	return math.Hypot(x-cx, y-cy) <= rad
}

func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}
