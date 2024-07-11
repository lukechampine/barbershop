package main

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
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

type audioBuffer struct {
	format          beep.Format
	samples         [][2]float64
	start, pos, end int

	r *beep.Resampler
	v *effects.Volume
}

func (ab *audioBuffer) seek(delta time.Duration) {
	ab.pos += ab.format.SampleRate.N(delta)
	ab.pos = max(ab.start, min(ab.pos, ab.end))
}

func (ab *audioBuffer) setRatio(r float64) {
	ab.r.SetRatio(r)
}

func (ab *audioBuffer) setVolume(v float64) {
	ab.v.Volume = v
}

func (ab *audioBuffer) times() (pos, total time.Duration) {
	return ab.format.SampleRate.D(ab.pos), ab.format.SampleRate.D(len(ab.samples))
}

func (ab *audioBuffer) Stream(samples [][2]float64) (n int, ok bool) {
	return ab.v.Stream(samples)
}

func (ab *audioBuffer) Err() error {
	return ab.v.Err()
}

func newAudioBuffer(format beep.Format, stream beep.Streamer) *audioBuffer {
	ab := &audioBuffer{format: format}
	for {
		var samples [512][2]float64
		n, ok := stream.Stream(samples[:])
		ab.samples = append(ab.samples, samples[:n]...)
		if !ok {
			break
		}
	}
	ab.end = len(ab.samples)
	abStream := func(samples [][2]float64) (n int, ok bool) {
		if ab.pos >= ab.end {
			ab.pos = ab.start
		}
		n = copy(samples, ab.samples[ab.pos:ab.end])
		ab.pos += n
		return n, true
	}
	ab.r = beep.ResampleRatio(4, 1.0, beep.StreamerFunc(abStream))
	ab.v = &effects.Volume{
		Streamer: ab.r,
		Base:     2,
		Volume:   0,
	}
	return ab
}

// global playback
var bb = struct {
	buf    *audioBuffer
	silent bool
}{}

func boomboxState() (pos, duration time.Duration, ratio float64) {
	speaker.Lock()
	defer speaker.Unlock()
	if bb.buf == nil {
		return 0, 0, 1
	}
	pos, duration = bb.buf.times()
	ratio = bb.buf.r.Ratio()
	return
}

func boomboxFadeIn(path string) error {
	if bb.silent {
		return nil
	}
	stream, format, err := openStreamer(path)
	if err != nil {
		return err
	}
	newBuf := newAudioBuffer(format, stream)
	newBuf.setVolume(-5)

	speaker.Lock()
	oldBuf := bb.buf
	bb.buf = newBuf
	speaker.Unlock()
	if oldBuf == nil {
		// crossfade with silence
		oldBuf = newAudioBuffer(format, beep.Silence(format.SampleRate.N(3*time.Second)))
		if err := speaker.Init(format.SampleRate, format.SampleRate.N(100*time.Millisecond)); err != nil {
			return err
		}
		speaker.Play(oldBuf)
	}
	speaker.Play(newBuf)
	for i := 0.0; i <= 50; i++ {
		speaker.Lock()
		newBuf.setVolume(-5 + (i * 0.1))
		oldBuf.setVolume(0 - (i * 0.1))
		speaker.Unlock()
		time.Sleep(40 * time.Millisecond)
	}
	speaker.Clear()
	speaker.Play(newBuf)
	return nil
}

func boomboxFadeOut() {
	if bb.silent {
		return
	}
	for i := 0.0; i <= 50; i++ {
		speaker.Lock()
		bb.buf.setVolume(0 - (i * 0.1))
		speaker.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	speaker.Lock()
	bb.buf.v.Silent = true
	speaker.Unlock()
	speaker.Clear()
}

func boomboxChangeSpeed(speedup float64) {
	if bb.silent {
		return
	}
	speaker.Lock()
	for math.Abs(speedup-bb.buf.r.Ratio()) > 0.01 {
		r := bb.buf.r.Ratio() + (speedup-bb.buf.r.Ratio())/10
		bb.buf.setRatio(r)
		speaker.Unlock()
		time.Sleep(100 * time.Millisecond)
		speaker.Lock()
	}
	bb.buf.setRatio(speedup)
	speaker.Unlock()
}

func boomboxSetSpeed(speedup float64) {
	if bb.silent {
		return
	}
	speaker.Lock()
	if bb.buf != nil {
		bb.buf.setRatio(speedup)
	}
	speaker.Unlock()
}

func boomboxSeek(delta time.Duration) {
	if bb.silent {
		return
	}
	speaker.Lock()
	if bb.buf != nil {
		bb.buf.seek(delta)
	}
	speaker.Unlock()
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
		for len(id.params) > 2 && id.params[1].ratio == r.params.ratio {
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
