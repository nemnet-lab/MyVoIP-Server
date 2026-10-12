package audio

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"testing"
	"time"
)

// makeWAV は正弦波の WAV を作る。
func makeWAV(t *testing.T, rate, channels, bits int, format uint16, freq float64, seconds float64, amp float64) []byte {
	t.Helper()
	frames := int(float64(rate) * seconds)
	bps := bits / 8
	data := make([]byte, frames*channels*bps)
	for i := 0; i < frames; i++ {
		v := amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate))
		for c := 0; c < channels; c++ {
			b := data[(i*channels+c)*bps:]
			switch {
			case format == 3:
				binary.LittleEndian.PutUint32(b, math.Float32bits(float32(v)))
			case bits == 8:
				b[0] = byte(int(v*127) + 128)
			case bits == 16:
				binary.LittleEndian.PutUint16(b, uint16(int16(v*32767)))
			case bits == 24:
				s := int32(v * 8388607)
				b[0], b[1], b[2] = byte(s), byte(s>>8), byte(s>>16)
			case bits == 32:
				binary.LittleEndian.PutUint32(b, uint32(int32(v*2147483647)))
			}
		}
	}
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(data)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], format)
	binary.LittleEndian.PutUint16(h[22:], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*channels*bps))
	binary.LittleEndian.PutUint16(h[32:], uint16(channels*bps))
	binary.LittleEndian.PutUint16(h[34:], uint16(bits))
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	return append(h, data...)
}

// readOutput は変換結果の WAV を確かめ、標本を返す。
func readOutput(t *testing.T, wav []byte) []int16 {
	t.Helper()
	if string(wav[0:4]) != "RIFF" || string(wav[8:16]) != "WAVEfmt " {
		t.Fatal("not a WAV")
	}
	if ch := binary.LittleEndian.Uint16(wav[22:]); ch != 1 {
		t.Fatalf("channels = %d", ch)
	}
	if rate := binary.LittleEndian.Uint32(wav[24:]); rate != SampleRate {
		t.Fatalf("rate = %d", rate)
	}
	if bits := binary.LittleEndian.Uint16(wav[34:]); bits != 16 {
		t.Fatalf("bits = %d", bits)
	}
	n := int(binary.LittleEndian.Uint32(wav[40:])) / 2
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(wav[44+i*2:]))
	}
	return pcm
}

// frequency はゼロ交差から周波数を見積もる（先頭と末尾のフィルターの立ち上がりを除く）。
func frequency(pcm []int16) float64 {
	body := pcm[400 : len(pcm)-400]
	crossings := 0
	for i := 1; i < len(body); i++ {
		if (body[i-1] < 0) != (body[i] < 0) {
			crossings++
		}
	}
	return float64(crossings) / 2 / (float64(len(body)) / SampleRate)
}

func peak(pcm []int16) float64 {
	var p float64
	for _, v := range pcm[400 : len(pcm)-400] {
		if a := math.Abs(float64(v)) / 32768; a > p {
			p = a
		}
	}
	return p
}

func TestConvertWAVFormats(t *testing.T) {
	cases := []struct {
		name                 string
		rate, channels, bits int
		format               uint16
	}{
		{"44.1kHz stereo 16bit", 44100, 2, 16, 1},
		{"48kHz mono 24bit", 48000, 1, 24, 1},
		{"16kHz mono 8bit", 16000, 1, 8, 1},
		{"8kHz mono 16bit", 8000, 1, 16, 1},
		{"96kHz stereo 32bit", 96000, 2, 32, 1},
		{"44.1kHz stereo float", 44100, 2, 32, 3},
		{"6kHz mono 16bit (upsample)", 6000, 1, 16, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wav := makeWAV(t, c.rate, c.channels, c.bits, c.format, 440, 2, 0.3)
			out, d, err := Convert(wav, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			pcm := readOutput(t, out)
			if d < 1990*time.Millisecond || d > 2010*time.Millisecond || math.Abs(float64(len(pcm))-16000) > 20 {
				t.Fatalf("duration %v, samples %d", d, len(pcm))
			}
			if f := frequency(pcm); math.Abs(f-440) > 5 {
				t.Fatalf("frequency = %.1f", f)
			}
			// 音量は -1dBFS 付近にそろう
			if p := peak(pcm); p < 0.85 || p > 0.9 {
				t.Fatalf("peak = %.3f", p)
			}
		})
	}
}

func TestConvertRemovesAliasing(t *testing.T) {
	// 6kHz は 8kHz にすると 2kHz に折り返す。低域通過フィルターで 60dB 以上弱める
	// （正規化で最大 8 倍に持ち上げても、0.3 の入力が 0.01 未満に収まる）
	wav := makeWAV(t, 48000, 1, 16, 1, 6000, 2, 0.3)
	out, _, err := Convert(wav, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if p := peak(readOutput(t, out)); p > 0.01 {
		t.Fatalf("aliased 6kHz tone peak = %.3f", p)
	}
}

func TestConvertMP3(t *testing.T) {
	data, err := os.ReadFile("testdata/sine1k.mp3") // 22.05kHz・ステレオ・1kHz・1.5秒（sox で作成）
	if err != nil {
		t.Fatal(err)
	}
	out, d, err := Convert(data, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pcm := readOutput(t, out)
	if d < 1400*time.Millisecond || d > 1700*time.Millisecond {
		t.Fatalf("duration = %v", d)
	}
	if f := frequency(pcm); math.Abs(f-1000) > 10 {
		t.Fatalf("frequency = %.1f", f)
	}
}

func TestConvertRejects(t *testing.T) {
	long := makeWAV(t, 8000, 1, 16, 1, 440, 3, 0.3)
	if _, _, err := Convert(long, 2*time.Second); !errors.Is(err, ErrTooLong) {
		t.Fatalf("too long: %v", err)
	}
	short := makeWAV(t, 8000, 1, 16, 1, 440, 0.5, 0.3)
	if _, _, err := Convert(short, time.Minute); !errors.Is(err, ErrTooShort) {
		t.Fatalf("too short: %v", err)
	}
	mp3, _ := os.ReadFile("testdata/sine1k.mp3")
	if _, _, err := Convert(mp3, time.Second); !errors.Is(err, ErrTooLong) {
		t.Fatalf("mp3 too long: %v", err)
	}
	for name, data := range map[string][]byte{
		"text":       []byte("hello, this is not audio"),
		"empty":      nil,
		"m4a":        append([]byte{0, 0, 0, 0x20}, []byte("ftypM4A ")...),
		"broken wav": []byte("RIFF\x00\x00\x00\x00WAVEjunk"),
		"adpcm wav":  func() []byte { b := makeWAV(t, 8000, 1, 16, 1, 440, 2, 0.3); b[20] = 2; return b }(),
	} {
		if _, _, err := Convert(data, time.Minute); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
