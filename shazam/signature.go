package shazam

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"time"

	"github.com/faiface/beep"
	"gonum.org/v1/gonum/dsp/fourier"
)

func convertSampleRate(x int) int {
	return map[int]int{
		1: 8000,
		2: 11025,
		3: 16000,
		4: 32000,
		5: 44100,

		8000:  1,
		11025: 2,
		16000: 3,
		32000: 4,
		44100: 5,
	}[x]
}

type frequencyPeak struct {
	pass      int
	magnitude int
	bin       int
}

// A Signature is a unique fingerprint of an audio sample.
type Signature struct {
	sampleRate  int
	numSamples  int
	peaksByBand [5][]frequencyPeak
}

func (s Signature) encode() (buf []byte) {
	write := func(u uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], u)
		buf = append(buf, b[:]...)
	}

	// header
	write(0xcafe2580)
	write(0) // checksum
	write(0) // length
	write(0x94119c00)
	write(0)
	write(0)
	write(0)
	write(uint32(convertSampleRate(s.sampleRate)) << 27)
	write(0)
	write(0)
	write(uint32(s.numSamples) + uint32(float64(s.sampleRate)*0.24))
	write(0x007c0000)
	write(uint32(0x40000000))
	write(0) // length2

	// peaks
	for band, peaks := range s.peaksByBand {
		if len(peaks) == 0 {
			continue
		}
		var peakBuf bytes.Buffer
		pass := 0
		for _, peak := range peaks {
			if peak.pass-pass >= 255 {
				peakBuf.WriteByte(0xFF)
				binary.Write(&peakBuf, binary.LittleEndian, uint32(peak.pass))
				pass = peak.pass
			}
			binary.Write(&peakBuf, binary.LittleEndian, uint8(peak.pass-pass))
			binary.Write(&peakBuf, binary.LittleEndian, uint16(peak.magnitude))
			binary.Write(&peakBuf, binary.LittleEndian, uint16(peak.bin))
			pass = peak.pass
		}
		write(uint32(0x60030040 + band))
		write(uint32(peakBuf.Len()))
		for peakBuf.Len()%4 != 0 {
			peakBuf.WriteByte(0x00)
		}
		buf = append(buf, peakBuf.Bytes()...)
	}

	binary.LittleEndian.PutUint32(buf[8:12], uint32(len(buf[48:])))
	binary.LittleEndian.PutUint32(buf[52:56], uint32(len(buf[48:])))
	binary.LittleEndian.PutUint32(buf[4:8], crc32.ChecksumIEEE(buf[8:]))
	return buf
}

func (s *Signature) decode(buf []byte) error {
	next := func() uint32 {
		v := binary.LittleEndian.Uint32(buf)
		buf = buf[4:]
		return v
	}

	// header
	if next() != 0xcafe2580 {
		return fmt.Errorf("bad magic1")
	} else if next() != crc32.ChecksumIEEE(buf[:]) {
		return fmt.Errorf("bad checksum")
	} else if next() != uint32(len(buf[36:])) {
		return fmt.Errorf("bad length")
	} else if next() != 0x94119c00 {
		return fmt.Errorf("bad magic2")
	}
	_, _, _ = next(), next(), next()
	s.sampleRate = convertSampleRate(int(next() >> 27))
	_, _ = next(), next()
	s.numSamples = int(next() - uint32(float64(s.sampleRate)*0.24))
	if next() != 0x007c0000 {
		return fmt.Errorf("bad magic3")
	} else if n := next(); n != 0x40000000 {
		fmt.Println(s.sampleRate, s.numSamples, n)
		return fmt.Errorf("bad magic4")
	} else if next() != uint32(len(buf))+8 {
		return fmt.Errorf("bad length2")
	}

	// peaks
	for len(buf) > 0 {
		band := int(next() - 0x60030040)
		size := next()
		peakBuf := bytes.NewReader(buf[:size])
		if size%4 != 0 {
			size += 4 - size%4
		}
		buf = buf[size:]

		var pass uint32
		for peakBuf.Len() > 0 {
			offset, _ := peakBuf.ReadByte()
			if offset == 0xFF {
				binary.Read(peakBuf, binary.LittleEndian, &pass)
				continue
			}
			pass += uint32(offset)
			var mag, bin uint16
			binary.Read(peakBuf, binary.LittleEndian, &mag)
			binary.Read(peakBuf, binary.LittleEndian, &bin)
			s.peaksByBand[band] = append(s.peaksByBand[band], frequencyPeak{
				pass:      int(pass),
				magnitude: int(mag),
				bin:       int(bin),
			})
		}
	}
	return nil
}

// CollectSample collects duration seconds of audio samples from s, starting at
// offset.
func CollectSample(s beep.Streamer, format beep.Format, offset, duration time.Duration) []float64 {
	samples := make([][2]float64, format.SampleRate.N(time.Second))
	rem := format.SampleRate.N(offset)
	for rem > 0 {
		n := len(samples)
		if n > rem {
			n = rem
		}
		n, ok := s.Stream(samples[:n])
		if !ok {
			break
		}
		rem -= n
	}
	rem = format.SampleRate.N(duration)
	mono := make([]float64, 0, rem)
	for rem > 0 {
		n := len(samples)
		if n > rem {
			n = rem
		}
		n, ok := s.Stream(samples[:n])
		if !ok {
			break
		}
		rem -= n
		for _, s := range samples[:n] {
			mono = append(mono, (s[0]+s[1])/2)
		}
	}
	return mono
}

type ring[T any] struct {
	buf   []T
	index int
}

func (r ring[T]) mod(i int) int {
	for i < 0 {
		i += len(r.buf)
	}
	return i % len(r.buf)
}

func (r ring[T]) At(i int) *T {
	return &r.buf[r.mod(r.index+i)]
}

func (r ring[T]) Append(x ...T) ring[T] {
	for len(x) > 0 {
		n := copy(r.buf[r.index:], x)
		x = x[n:]
		r.index = (r.index + n) % len(r.buf)
	}
	return r
}

func (r ring[T]) Slice(s []T, offset int) {
	offset = r.mod(offset + r.index)
	for len(s) > 0 {
		n := copy(s, r.buf[offset:])
		s = s[n:]
		offset = (offset + n) % len(r.buf)
	}
}

func newRing[T any](size int) ring[T] {
	return ring[T]{buf: make([]T, size)}
}

// ComputeSignature computes the audio signature of the provided samples.
func ComputeSignature(sampleRate int, samples []float64) Signature {
	maxNeighbor := func(spreadOutputs ring[[1025]float64], i int) (neighbor float64) {
		for _, off := range []int{-10, -7, -4, -3, 1, 2, 5, 8} {
			neighbor = max(neighbor, spreadOutputs.At(-49)[(i+off)])
		}
		for _, off := range []int{-53, -45, 165, 172, 179, 186, 193, 200, 214, 221, 228, 235, 242, 249} {
			neighbor = max(neighbor, spreadOutputs.At(off)[i-1])
		}
		return neighbor
	}
	normalizePeak := func(x float64) float64 {
		return math.Log(max(x, 1.0/64))*1477.3 + 6144
	}
	peakBand := func(bin int) (int, bool) {
		hz := (bin * sampleRate) / (2 * 1024 * 64)
		band, ok := map[bool]int{
			250 <= hz && hz < 520:    0,
			520 <= hz && hz < 1450:   1,
			1450 <= hz && hz < 3500:  2,
			3500 <= hz && hz <= 5500: 3,
		}[true]
		return band, ok
	}

	fft := fourier.NewFFT(2048)
	samplesRing := newRing[float64](2048)
	fftOutputs := newRing[[1025]float64](256)
	spreadOutputs := newRing[[1025]float64](256)
	var peaksByBand [5][]frequencyPeak
	for i := 0; i*128+128 < len(samples); i++ {
		samplesRing = samplesRing.Append(samples[i*128:][:128]...)

		// Perform FFT
		reorderedSamples := make([]float64, 2048)
		samplesRing.Slice(reorderedSamples, 0)
		for i, m := range hanningMultipliers {
			reorderedSamples[i] = math.Round(reorderedSamples[i]*1024*64) * m
		}
		var outputs [1025]float64
		for i, c := range fft.Coefficients(nil, reorderedSamples) {
			outputs[i] = max((real(c)*real(c)+imag(c)*imag(c))/(1<<17), 0.0000000001)
		}
		fftOutputs = fftOutputs.Append(outputs)

		// Spread peaks, both in the frequency domain...
		for i := 0; i < len(outputs)-2; i++ {
			outputs[i] = max(outputs[i], outputs[i+1], outputs[i+2])
		}
		spreadOutputs = spreadOutputs.Append(outputs)
		// ... and in the time domain
		for _, off := range []int{-2, -4, -7} {
			prev := spreadOutputs.At(off)
			for i := range prev {
				prev[i] = max(prev[i], outputs[i])
			}
		}

		// Accumulate samples until we have enough...
		if i < 45 {
			continue
		}
		// ...then recognize peaks
		fftOutput := fftOutputs.At(-46)
		for bin := 10; bin < 1015; bin++ {
			// Ensure that this is a frequency- and time-domain local maximum
			if fftOutput[bin] <= maxNeighbor(spreadOutputs, bin) {
				continue
			}
			// Normalize and compute frequency band
			before := normalizePeak(fftOutput[bin-1])
			peak := normalizePeak(fftOutput[bin])
			after := normalizePeak(fftOutput[bin+1])
			variation := int((32 * (after - before)) / (2*peak - after - before))
			peakBin := bin*64 + variation
			band, ok := peakBand(peakBin)
			if !ok {
				continue
			}
			peaksByBand[band] = append(peaksByBand[band], frequencyPeak{
				pass:      i - 45,
				magnitude: int(peak),
				bin:       peakBin,
			})
		}
	}
	return Signature{
		sampleRate:  sampleRate,
		numSamples:  len(samples),
		peaksByBand: peaksByBand,
	}
}
