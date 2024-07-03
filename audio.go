package main

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/faiface/beep"
	"github.com/faiface/beep/effects"
	"github.com/faiface/beep/mp3"
	"github.com/faiface/beep/speaker"
	"github.com/faiface/beep/vorbis"
	"github.com/faiface/beep/wav"
	"lukechampine.com/barbershop/shazam"
)

func openStreamer(path string) (beep.StreamSeekCloser, beep.Format, error) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	mimeBuf := make([]byte, 512)
	if _, err := f.ReadAt(mimeBuf, 0); err != nil {
		log.Fatalln("Could not detect audio format:", err)
	}
	mime := http.DetectContentType(mimeBuf)
	switch mime {
	case "audio/wave":
		return wav.Decode(f)
	case "audio/mpeg":
		return mp3.Decode(f)
	case "application/ogg":
		return vorbis.Decode(f)
	default:
		return nil, beep.Format{}, fmt.Errorf("unsupported mime type: %s", mime)
	}
}

// global playback
var bb = struct {
	r      *beep.Resampler
	v      *effects.Volume
	silent bool
	mu     sync.Mutex

	offset  time.Duration
	ratio   float64
	stateMu sync.Mutex
}{
	ratio: 1,
}

func boomboxState() (time.Duration, float64) {
	bb.stateMu.Lock()
	defer bb.stateMu.Unlock()
	return bb.offset, bb.ratio
}

func boomboxFadeIn(path string) error {
	if bb.silent {
		return nil
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	stream, format, err := openStreamer(path)
	if err != nil {
		return err
	}
	oldv := bb.v
	if oldv == nil {
		// crossfade with silence
		oldv = &effects.Volume{
			Streamer: beep.Silence(-1),
			Base:     2,
			Volume:   0,
		}
		if err := speaker.Init(format.SampleRate, format.SampleRate.N(100*time.Millisecond)); err != nil {
			return err
		}
		speaker.Play(oldv)
	}
	newr := beep.ResampleRatio(4, 1.0, stream)
	newv := &effects.Volume{
		Streamer: newr,
		Base:     2,
		Volume:   -5,
	}
	speaker.Play(beep.StreamerFunc(func(samples [][2]float64) (int, bool) {
		n, ok := newv.Stream(samples)
		bb.stateMu.Lock()
		bb.offset += format.SampleRate.D(n)
		bb.stateMu.Unlock()
		return n, ok
	}))
	for oldv.Volume > -5 {
		speaker.Lock()
		oldv.Volume -= 0.1
		newv.Volume += 0.1
		speaker.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	speaker.Clear()
	speaker.Play(beep.StreamerFunc(func(samples [][2]float64) (int, bool) {
		n, ok := newv.Stream(samples)
		bb.stateMu.Lock()
		bb.offset += format.SampleRate.D(n)
		bb.stateMu.Unlock()
		return n, ok
	}))
	bb.r = newr
	bb.v = newv
	return nil
}

func boomboxFadeOut() {
	if bb.silent {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	for bb.v.Volume > 0 {
		speaker.Lock()
		bb.v.Volume -= 0.1
		speaker.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	bb.v.Silent = true
	speaker.Clear()
}

func boomboxChangeSpeed(speedup float64) {
	if bb.silent {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	for math.Abs(speedup-bb.r.Ratio()) > 0.01 {
		speaker.Lock()
		r := bb.r.Ratio() + (speedup-bb.r.Ratio())/10
		bb.r.SetRatio(r)
		speaker.Unlock()
		bb.stateMu.Lock()
		bb.ratio = r
		bb.stateMu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	speaker.Lock()
	bb.r.SetRatio(speedup)
	speaker.Unlock()
	bb.stateMu.Lock()
	bb.ratio = speedup
	bb.stateMu.Unlock()
}

type identifyParams struct {
	ratio  float64
	offset time.Duration
}

type identifyResult struct {
	params identifyParams
	res    shazam.Result
	skew   float64
}

func identifyPath(path string, params identifyParams) (identifyResult, error) {
	stream, format, err := openStreamer(path)
	if err != nil {
		return identifyResult{}, err
	}
	s := beep.ResampleRatio(6, params.ratio*float64(format.SampleRate)/16000, stream)
	format.SampleRate = 16000
	sample := shazam.CollectSample(s, format, params.offset, 12*time.Second)
	res, err := shazam.Identify(shazam.ComputeSignature(int(format.SampleRate), sample))
	if err != nil {
		return identifyResult{}, err
	}
	return identifyResult{
		params: params,
		res:    res,
		skew:   min(10*math.Abs(res.Skew), 1),
	}, nil
}

type trackIdentifier struct {
	path    string
	params  []identifyParams
	results []identifyResult
	sample  *identifyResult
}

func newTrackIdentifier(path string) *trackIdentifier {
	var params []identifyParams
	for _, speedup := range []float64{1.20, 1.30, 1.10, 1.25, 1.15, 1.40, 1.50, 0.90, 0.80, 1.60, 1.70, 1.80, 1.90, 2.00, 1.00} {
		for _, offset := range []time.Duration{24 * time.Second, 48 * time.Second, 72 * time.Second} {
			params = append(params, identifyParams{speedup, offset})
		}
	}
	return &trackIdentifier{
		path:   path,
		params: params,
	}
}

func (id *trackIdentifier) currentParams() identifyParams {
	return id.params[0]
}

func (id *trackIdentifier) handleResult(r identifyResult) (nextParams *identifyParams) {
	if !r.res.Found {
		// skip to the next speedup without trying other offsets
		for len(id.params) > 1 && id.params[0].ratio == r.params.ratio {
			id.params = id.params[1:]
		}
	} else {
		// have we found a match?
		id.results = append(id.results, r)
		hits := 0
		for _, res := range id.results {
			if res.res.Artist == r.res.Artist && res.res.Title == r.res.Title {
				hits++
			}
		}
		if hits == 3 {
			id.sample = &r
			return nil
		}
	}

	// have we exhausted all params?
	if len(id.params) == 1 {
		return nil
	}
	id.params = id.params[1:]
	p := id.params[0]
	return &p
}
