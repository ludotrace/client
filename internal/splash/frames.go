package splash

import (
	"embed"
	"image"
	"image/png"
	"sort"
	"strings"
	"time"
)

// cycle is the total playback duration of the splash: one heartbeat cycle at 2x
// speed. The site's locked motion is 4.2s (site/index.html --heartbeat-t); this
// is a uniform time-scale to half that. frameInterval is derived so
// frameInterval * frameCount == cycle, which is what makes the 2x compression a
// pure time-scale — the pre-rendered frames sample the same keyframe
// proportions, just played back over the shorter duration.
const cycle = 2100 * time.Millisecond

//go:generate go run gen/main.go assets

//go:embed assets/frame*.png
var frameFS embed.FS

// loadFrames decodes the embedded PNG sequence in filename order and returns
// the frames plus the per-frame interval. The frames are the pre-rendered
// heartbeat cycle (see gen/main.go). Decoding is done once, on demand, from the
// Windows splash path only.
func loadFrames() ([]*image.RGBA, time.Duration, error) {
	entries, err := frameFS.ReadDir("assets")
	if err != nil {
		return nil, 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "frame") && strings.HasSuffix(e.Name(), ".png") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	frames := make([]*image.RGBA, 0, len(names))
	for _, n := range names {
		f, err := frameFS.Open("assets/" + n)
		if err != nil {
			return nil, 0, err
		}
		img, err := png.Decode(f)
		f.Close()
		if err != nil {
			return nil, 0, err
		}
		frames = append(frames, toRGBA(img))
	}

	interval := cycle
	if len(frames) > 0 {
		interval = cycle / time.Duration(len(frames))
	}
	return frames, interval, nil
}

// toRGBA returns img as *image.RGBA, converting only if needed.
func toRGBA(img image.Image) *image.RGBA {
	if r, ok := img.(*image.RGBA); ok {
		return r
	}
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(x, y, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
