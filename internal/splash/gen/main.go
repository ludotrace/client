//go:build ignore

// Command gen renders the launch splash's heartbeat animation to a PNG frame
// sequence under ../assets, which internal/splash embeds via //go:embed.
//
// Why pre-render rather than draw at runtime: the Windows client is
// CGO_ENABLED=0 with no SVG/vector stack, and the runtime splash path must stay
// tiny and dependency-free (a raw Win32 window that just blits bitmaps). This
// generator is the one place the org's hero heartbeat mark is turned into
// pixels — pure Go, deterministic, no external rasterizer — mirroring how the
// tray icons are baked and embedded.
//
// The motion is a uniform 2x time-scale of the site's locked 4.2s heartbeat
// (internal/_bmad-output/.../DESIGN.md, site/index.html --heartbeat-t: 4.2s):
// the same keyframe proportions (beam sweeps 5%->70% of the cycle, each marker
// flashes to 1.35x as the beam arrives then settles to its avatar baseline
// opacity) played over half the duration. Keyframe positions are expressed as
// fractions of the cycle, so halving the playback duration (frame interval x
// frameCount = ~2.1s) preserves every beat's relative position. It is not a
// re-choreograph.
//
// Geometry is the canonical avatar mark (internal/brand/avatar_heartbeat_v3.svg,
// 680x680 art): the triple-stroke polyline plus its six diamond/square markers,
// on the #12002e void background, square caps, no gradients/rounded — per the
// brand rules in internal/brand/CLAUDE.md.
//
// Run with: go run ./gen  (from internal/splash), or `go generate ./...`.
package main

import (
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const (
	art        = 680.0 // avatar art canvas (viewBox units)
	size       = 220   // rendered splash frame edge, px
	frameCount = 30    // frames per cycle; interval is set in frames.go (~2.1s / 30)

	// Beam sweep window as a fraction of the cycle, from the locked keyframes
	// (site @keyframes beam: visible 5%..70%).
	beamStart = 0.05
	beamEnd   = 0.70

	// Global dismiss fade so the window close isn't abrupt.
	fadeStart = 0.90
)

// rgb is a straight (non-premultiplied) colour.
type rgb struct{ r, g, b float64 }

var (
	bg        = rgb{0x12, 0x00, 0x2e}
	glowCol   = rgb{0x3a, 0x00, 0xff} // outer stroke
	signalCol = rgb{0x00, 0xe5, 0xff} // main line
	hiCol     = rgb{0xa0, 0xf0, 0xff} // highlight + beam
	eventCol  = rgb{0xff, 0x2d, 0x78} // magenta markers
	dataCol   = rgb{0xc7, 0x7d, 0xff} // purple markers
)

type pt struct{ x, y float64 }

// The heartbeat polyline, in art coords.
var poly = []pt{{80, 340}, {270, 340}, {310, 160}, {350, 480}, {390, 340}, {600, 340}}

type markerKind int

const (
	diamond markerKind = iota
	square
)

type marker struct {
	c        pt         // centre, art coords
	r        float64    // half-extent, art units (radius for diamond, half-side for square)
	col      rgb        // fill
	baseline float64    // rest opacity (from the avatar mark)
	kind     markerKind //
}

// The six avatar markers, centres/opacities matching avatar_heartbeat_v3.svg.
var markers = []marker{
	{c: pt{310, 160}, r: 24, col: eventCol, baseline: 1.00, kind: diamond}, // peak
	{c: pt{350, 482}, r: 16, col: eventCol, baseline: 0.80, kind: square},  // trough
	{c: pt{174, 340}, r: 14, col: dataCol, baseline: 0.90, kind: square},
	{c: pt{492, 340}, r: 24, col: dataCol, baseline: 0.85, kind: diamond},
	{c: pt{562, 340}, r: 14, col: dataCol, baseline: 0.50, kind: square},
	{c: pt{132, 340}, r: 24, col: dataCol, baseline: 0.35, kind: diamond},
}

func main() {
	outDir := "assets"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err)
	}

	// Precompute cumulative arc length and each marker's arc fraction along the
	// path, so its flash fires when the beam actually reaches it.
	segLen, total := arcLengths()
	markerFrac := make([]float64, len(markers))
	for i, m := range markers {
		markerFrac[i] = projectFrac(m.c, segLen, total)
	}

	for f := 0; f < frameCount; f++ {
		c := float64(f) / float64(frameCount-1) // cycle progress 0..1
		img := renderFrame(c, segLen, total, markerFrac)
		name := filepath.Join(outDir, fmt.Sprintf("frame%02d.png", f))
		writePNG(name, img)
	}
	fmt.Printf("wrote %d frames (%dx%d) to %s\n", frameCount, size, size, outDir)
}

func renderFrame(c float64, segLen []float64, total float64, markerFrac []float64) *image.RGBA {
	s := float64(size) / art
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// Background.
	fill(img, bg)

	// Static triple stroke (glow -> signal -> highlight), painted every frame.
	drawPolyline(img, poly, 16*s*0.5, glowCol, 0.35, s)
	drawPolyline(img, poly, 5*s*0.5, signalCol, 1.0, s)
	drawPolyline(img, poly, 1.5*s*0.5, hiCol, 0.5, s)

	// Beam: a bright dash travelling along the path over 5%..70% of the cycle.
	if c >= beamStart && c <= beamEnd {
		bf := (c - beamStart) / (beamEnd - beamStart) // 0..1 along the path
		drawBeam(img, poly, segLen, total, bf, s)
	}

	// Markers flash as the beam arrives, then settle to baseline opacity.
	for i, m := range markers {
		popC := beamStart + markerFrac[i]*(beamEnd-beamStart)
		op, sc := markerAnim(c, popC, m.baseline)
		if op <= 0 {
			continue
		}
		drawMarker(img, m, op, sc, s)
	}

	// Global dismiss fade toward the void.
	if c > fadeStart {
		k := (c - fadeStart) / (1 - fadeStart) // 0..1
		fadeToward(img, bg, k)
	}
	return img
}

// markerAnim reproduces the f-keyframe shape (scaled): hidden until just before
// popC, a flash to 1.35x at full opacity, then a settle to the marker's
// baseline opacity at unit scale.
func markerAnim(c, popC, baseline float64) (opacity, scale float64) {
	const (
		rampIn = 0.02 // grow-in before arrival
		settle = 0.05 // flash -> baseline after arrival
	)
	switch {
	case c < popC-rampIn:
		return 0, 0
	case c < popC:
		k := (c - (popC - rampIn)) / rampIn // 0..1
		return k, 0.3 + k*(1.35-0.3)
	case c < popC+settle:
		k := (c - popC) / settle // 0..1
		return 1 + k*(baseline-1), 1.35 + k*(1.0-1.35)
	default:
		return baseline, 1.0
	}
}

// ---- rasterisation helpers -------------------------------------------------

func fill(img *image.RGBA, c rgb) {
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i+0] = uint8(c.r)
		img.Pix[i+1] = uint8(c.g)
		img.Pix[i+2] = uint8(c.b)
		img.Pix[i+3] = 0xff
	}
}

// over composites straight colour src at coverage a (0..1) onto the pixel.
func over(img *image.RGBA, x, y int, src rgb, a float64) {
	if a <= 0 || x < 0 || y < 0 || x >= size || y >= size {
		return
	}
	if a > 1 {
		a = 1
	}
	i := img.PixOffset(x, y)
	dr := float64(img.Pix[i+0])
	dg := float64(img.Pix[i+1])
	db := float64(img.Pix[i+2])
	img.Pix[i+0] = uint8(src.r*a + dr*(1-a))
	img.Pix[i+1] = uint8(src.g*a + dg*(1-a))
	img.Pix[i+2] = uint8(src.b*a + db*(1-a))
	img.Pix[i+3] = 0xff
}

// drawPolyline strokes the path with a round-ish anti-aliased line of the given
// half-width (px) and colour at the given opacity.
func drawPolyline(img *image.RGBA, p []pt, halfPx float64, col rgb, op, s float64) {
	for i := 0; i+1 < len(p); i++ {
		drawSeg(img, p[i].x*s, p[i].y*s, p[i+1].x*s, p[i+1].y*s, halfPx, col, op)
	}
}

func drawSeg(img *image.RGBA, x0, y0, x1, y1, halfPx float64, col rgb, op float64) {
	minX := int(math.Floor(math.Min(x0, x1) - halfPx - 1))
	maxX := int(math.Ceil(math.Max(x0, x1) + halfPx + 1))
	minY := int(math.Floor(math.Min(y0, y1) - halfPx - 1))
	maxY := int(math.Ceil(math.Max(y0, y1) + halfPx + 1))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			d := segDist(float64(x)+0.5, float64(y)+0.5, x0, y0, x1, y1)
			cov := halfPx + 0.5 - d // 1px AA falloff
			if cov <= 0 {
				continue
			}
			if cov > 1 {
				cov = 1
			}
			over(img, x, y, col, cov*op)
		}
	}
}

// drawBeam paints a short bright dash centred at arc-fraction bf along the path,
// with a soft glow, giving the "signal travelling" look.
func drawBeam(img *image.RGBA, p []pt, segLen []float64, total, bf, s float64) {
	center := bf * total
	const halfArt = 34.0 // half dash length, art units
	// Sample points densely along the dash and stamp glow dots.
	steps := 40
	for i := 0; i <= steps; i++ {
		a := center - halfArt + (2*halfArt)*float64(i)/float64(steps)
		if a < 0 || a > total {
			continue
		}
		pos := pointAt(p, segLen, a)
		// taper toward the ends of the dash
		t := 1 - math.Abs(a-center)/halfArt // 1 at centre, 0 at ends
		if t < 0 {
			t = 0
		}
		drawDot(img, pos.x*s, pos.y*s, 6*s, hiCol, 0.9*t)
		drawDot(img, pos.x*s, pos.y*s, 2.5*s, rgb{255, 255, 255}, t)
	}
}

func drawDot(img *image.RGBA, cx, cy, rPx float64, col rgb, op float64) {
	minX := int(math.Floor(cx - rPx - 1))
	maxX := int(math.Ceil(cx + rPx + 1))
	minY := int(math.Floor(cy - rPx - 1))
	maxY := int(math.Ceil(cy + rPx + 1))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			cov := rPx + 0.5 - d
			if cov <= 0 {
				continue
			}
			if cov > 1 {
				cov = 1
			}
			over(img, x, y, col, cov*op)
		}
	}
}

func drawMarker(img *image.RGBA, m marker, op, scale, s float64) {
	r := m.r * scale
	cx, cy := m.c.x, m.c.y
	minX := int(math.Floor((cx-r)*s - 1))
	maxX := int(math.Ceil((cx+r)*s + 1))
	minY := int(math.Floor((cy-r)*s - 1))
	maxY := int(math.Ceil((cy+r)*s + 1))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			ax := (float64(x)+0.5)/s - cx
			ay := (float64(y)+0.5)/s - cy
			var dist float64 // signed distance outside the shape, art units
			switch m.kind {
			case diamond:
				dist = (math.Abs(ax) + math.Abs(ay)) - r
			default: // square
				dist = math.Max(math.Abs(ax), math.Abs(ay)) - r
			}
			cov := (0.5/s - dist) * s // ~1px AA in device space
			if cov <= 0 {
				continue
			}
			if cov > 1 {
				cov = 1
			}
			over(img, x, y, m.col, cov*op)
		}
	}
}

func fadeToward(img *image.RGBA, c rgb, k float64) {
	if k <= 0 {
		return
	}
	if k > 1 {
		k = 1
	}
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i+0] = uint8(float64(img.Pix[i+0])*(1-k) + c.r*k)
		img.Pix[i+1] = uint8(float64(img.Pix[i+1])*(1-k) + c.g*k)
		img.Pix[i+2] = uint8(float64(img.Pix[i+2])*(1-k) + c.b*k)
	}
}

// ---- path geometry ---------------------------------------------------------

func arcLengths() (segLen []float64, total float64) {
	segLen = make([]float64, len(poly)-1)
	for i := 0; i+1 < len(poly); i++ {
		segLen[i] = math.Hypot(poly[i+1].x-poly[i].x, poly[i+1].y-poly[i].y)
		total += segLen[i]
	}
	return segLen, total
}

// pointAt returns the point at arc length a along the path.
func pointAt(p []pt, segLen []float64, a float64) pt {
	acc := 0.0
	for i := 0; i+1 < len(p); i++ {
		if a <= acc+segLen[i] || i == len(p)-2 {
			t := 0.0
			if segLen[i] > 0 {
				t = (a - acc) / segLen[i]
			}
			return pt{p[i].x + (p[i+1].x-p[i].x)*t, p[i].y + (p[i+1].y-p[i].y)*t}
		}
		acc += segLen[i]
	}
	return p[len(p)-1]
}

// projectFrac returns the arc fraction (0..1) of the point on the path nearest
// to c — where along the sweep this marker lights up.
func projectFrac(c pt, segLen []float64, total float64) float64 {
	best, bestA := math.Inf(1), 0.0
	acc := 0.0
	for i := 0; i+1 < len(poly); i++ {
		x0, y0 := poly[i].x, poly[i].y
		x1, y1 := poly[i+1].x, poly[i+1].y
		dx, dy := x1-x0, y1-y0
		l2 := dx*dx + dy*dy
		t := 0.0
		if l2 > 0 {
			t = ((c.x-x0)*dx + (c.y-y0)*dy) / l2
		}
		t = math.Max(0, math.Min(1, t))
		px, py := x0+dx*t, y0+dy*t
		d := math.Hypot(c.x-px, c.y-py)
		if d < best {
			best = d
			bestA = acc + t*segLen[i]
		}
		acc += segLen[i]
	}
	if total == 0 {
		return 0
	}
	return bestA / total
}

func segDist(px, py, x0, y0, x1, y1 float64) float64 {
	dx, dy := x1-x0, y1-y0
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(px-x0, py-y0)
	}
	t := ((px-x0)*dx + (py-y0)*dy) / l2
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(x0+dx*t), py-(y0+dy*t))
}

func writePNG(name string, img *image.RGBA) {
	f, err := os.Create(name)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(f, img); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gen:", err)
	os.Exit(1)
}
