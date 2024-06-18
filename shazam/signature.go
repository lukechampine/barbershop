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

func frequencyBand(hz int) int {
	switch {
	case hz < 250:
		return -1
	case hz < 520:
		return 0
	case hz < 1450:
		return 1
	case hz < 3500:
		return 2
	case hz <= 5500:
		return 3
	default:
		return -1
	}
}

type frequencyPeak struct {
	pass      int
	magnitude int
	bin       int
}

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

func ComputeSignature(sampleRate int, samples []float64) Signature {
	var samplesRing [2048]float64
	var samplesIndex int
	var reorderedSamplesRing [2048]float64
	var fftOutputs [256][1025]float64
	var fftOutputsIndex int
	var fft *fourier.FFT = fourier.NewFFT(2048)
	var spreadFFTOutputs [256][1025]float64
	var spreadFFTOutputsIndex int
	var numSpreadFFTsDone int
	sig := Signature{
		sampleRate:  sampleRate,
		numSamples:  len(samples),
		peaksByBand: [5][]frequencyPeak{},
	}

	doFFT := func(s16_mono_16khz_buffer []float64) {
		for i := range s16_mono_16khz_buffer {
			s16_mono_16khz_buffer[i] = math.Round(s16_mono_16khz_buffer[i] * 65536) // XX
		}
		copy(samplesRing[samplesIndex:samplesIndex+128], s16_mono_16khz_buffer)
		samplesIndex = (samplesIndex + 128) % len(samplesRing)

		// Reorder the items (put the latest data at end) and apply Hanning window
		for i, m := range hanning_multipliers {
			reorderedSamplesRing[i] = samplesRing[(i+samplesIndex)%len(samplesRing)] * m
		}

		// Perform Fast Fourier transform
		complex_fft_results := fft.Coefficients(nil, reorderedSamplesRing[:])
		if len(complex_fft_results) != 1025 {
			panic("unexpected length")
		}

		// Turn complex into reals, and put the results into a local array
		for i := range complex_fft_results {
			re, im := real(complex_fft_results[i]), imag(complex_fft_results[i])
			fftOutputs[fftOutputsIndex][i] = max((re*re+im*im)/(1<<17), 0.0000000001)
		}
		fftOutputsIndex = (fftOutputsIndex + 1) % len(fftOutputs)
	}

	spreadPeaks := func() {
		spread_fft_results := fftOutputs[(fftOutputsIndex-1+256)%256]

		// Perform frequency-domain spreading of peak values
		for i := 0; i < 1023; i++ {
			spread_fft_results[i] = max(spread_fft_results[i], spread_fft_results[i+1], spread_fft_results[i+2])
		}

		// Perform time-domain spreading of peak values
		for i := 0; i < 1025; i++ {
			max_value := spread_fft_results[i]
			for _, j := range []int{1, 3, 6} {
				former_fft_output := &spreadFFTOutputs[((spreadFFTOutputsIndex - j + 256) % 256)]
				max_value = max(former_fft_output[i], max_value)
				former_fft_output[i] = max_value
			}
		}

		spreadFFTOutputs[spreadFFTOutputsIndex] = spread_fft_results
		spreadFFTOutputsIndex = (spreadFFTOutputsIndex + 1) % 256
	}

	doPeakRecognition := func() {
		// Note: when substracting an array index, casting to signed is needed
		// to avoid underflow panics at runtime.

		fft_minus_46 := &fftOutputs[((fftOutputsIndex - 46 + 256) % 256)]
		fft_minus_49 := &spreadFFTOutputs[((spreadFFTOutputsIndex - 49 + 256) % 256)]

		for bin_position := 10; bin_position < 1015; bin_position++ {

			// Ensure that the bin is large enough to be a peak
			if fft_minus_46[bin_position] >= 1.0/64.0 && fft_minus_46[bin_position] >= fft_minus_49[bin_position-1] {

				// Ensure that it is frequency-domain local minimum
				max_neighbor_in_fft_minus_49 := 0.0

				for _, neighbor_offset := range []int{-10, -7, -4, -3, 1, 2, 5, 8} {
					max_neighbor_in_fft_minus_49 = max(max_neighbor_in_fft_minus_49, fft_minus_49[(bin_position+neighbor_offset)])
				}

				if fft_minus_46[bin_position] > max_neighbor_in_fft_minus_49 {
					// Ensure that it is a time-domain local minimum
					max_neighbor_in_other_adjacent_ffts := max_neighbor_in_fft_minus_49

					for _, other_offset := range []int{-53, -45, 165, 172, 179, 186, 193, 200, 214, 221, 228, 235, 242, 249} {
						other_fft := &spreadFFTOutputs[((spreadFFTOutputsIndex + other_offset + 256) % 256)]
						max_neighbor_in_other_adjacent_ffts = max(max_neighbor_in_other_adjacent_ffts, other_fft[bin_position-1])
					}

					if fft_minus_46[bin_position] > max_neighbor_in_other_adjacent_ffts {
						// This is a peak, store the peak
						fft_pass_number := numSpreadFFTsDone - 46

						peak_magnitude := math.Log(max(fft_minus_46[bin_position], 1.0/64.0))*1477.3 + 6144.0
						peak_magnitude_before := math.Log(max(fft_minus_46[bin_position-1], 1.0/64.0))*1477.3 + 6144.0
						peak_magnitude_after := math.Log(max(fft_minus_46[bin_position+1], 1.0/64.0))*1477.3 + 6144.0

						peak_variation_1 := peak_magnitude*2.0 - peak_magnitude_before - peak_magnitude_after
						peak_variation_2 := (peak_magnitude_after - peak_magnitude_before) * 32.0 / peak_variation_1
						if peak_variation_1 < 0 {
							panic("unexpected")
						}

						corrected_peak_frequency_bin := int((float64(bin_position*64) + (peak_variation_2)))

						// Convert back a FFT bin to a frequency, given a 16 KHz sample
						// rate, 1024 useful bins and the multiplication by 64 made before
						// storing the information
						frequency_hz := int(float64(corrected_peak_frequency_bin) * (float64(sampleRate) / 2.0 / 1024.0 / 64.0))

						// Ignore peaks outside the 250 Hz-5.5 KHz range, store them into
						// a lookup table that will be used to generate the binary fingerprint
						// otherwise
						frequency_band := frequencyBand(frequency_hz)
						if frequency_band == -1 {
							continue
						}
						sig.peaksByBand[frequency_band] = append(sig.peaksByBand[frequency_band], frequencyPeak{
							pass:      fft_pass_number,
							magnitude: int(peak_magnitude),
							bin:       corrected_peak_frequency_bin,
						})
					}
				}
			}
		}
	}

	for i := 0; i+128 < len(samples); i += 128 {
		doFFT(samples[i : i+128])
		spreadPeaks()
		if numSpreadFFTsDone++; numSpreadFFTsDone >= 46 {
			doPeakRecognition()
		}
	}
	return sig
}
