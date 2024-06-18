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
var bb struct {
	r      *beep.Resampler
	v      *effects.Volume
	silent bool
	mu     sync.Mutex
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
		if err := speaker.Init(format.SampleRate, format.SampleRate.N(10*time.Millisecond)); err != nil {
			return err
		}
		speaker.Play(oldv)
	}
	bb.r = beep.ResampleRatio(4, 1.0, stream)
	bb.v = &effects.Volume{
		Streamer: bb.r,
		Base:     2,
		Volume:   -5,
	}
	speaker.Play(bb.v)
	for oldv.Volume > -5 {
		speaker.Lock()
		oldv.Volume -= 0.1
		bb.v.Volume += 0.1
		speaker.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	speaker.Clear()
	speaker.Play(bb.v)
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
		bb.r.SetRatio(bb.r.Ratio() + (speedup-bb.r.Ratio())/10)
		speaker.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
}

type identifyResult struct {
	res   shazam.Result
	ratio float64
	skew  float64
}

func identifyPath(path string, speedup float64, offset time.Duration) (identifyResult, error) {
	stream, format, err := openStreamer(path)
	if err != nil {
		return identifyResult{}, err
	}
	s := beep.ResampleRatio(6, speedup*float64(format.SampleRate)/16000, stream)
	format.SampleRate = 16000
	sample := shazam.CollectSample(s, format, offset, 12*time.Second)
	res, err := shazam.Identify(shazam.ComputeSignature(int(format.SampleRate), sample))
	if err != nil {
		return identifyResult{}, err
	}
	return identifyResult{
		res:   res,
		ratio: speedup,
		skew:  math.Abs(res.Skew),
	}, nil
}
