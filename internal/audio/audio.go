// Package audio は保留音のファイル（WAV・MP3）を、中継用 Asterisk がそのまま再生できる
// 8kHz・モノラル・16bit の WAV に変換する。外部のコマンド（ffmpeg など）は使わない。
package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	mp3 "github.com/hajimehoshi/go-mp3"
)

// SampleRate は変換後のサンプリング周波数（電話の音声帯域）。
const SampleRate = 8000

var (
	// ErrUnsupported は WAV・MP3 以外か、読めない形式。
	ErrUnsupported = errors.New("unsupported audio format")
	// ErrTooLong は長すぎる音声。
	ErrTooLong = errors.New("audio is too long")
	// ErrTooShort は短すぎる（または空の）音声。
	ErrTooShort = errors.New("audio is too short")
)

// MinDuration より短い音声は受け付けない。
const MinDuration = time.Second

// Convert は WAV か MP3 を読み、8kHz・モノラル・16bit の WAV と長さを返す。
// 音量は最大値が -1dBFS 付近になるようにそろえる（小さすぎる・割れている音声を電話で聞きやすくする）。
func Convert(data []byte, maxDuration time.Duration) ([]byte, time.Duration, error) {
	var pcm []int16
	var err error
	switch {
	case isWAV(data):
		pcm, err = convertWAV(data, maxDuration)
	case isMP3(data):
		pcm, err = convertMP3(data, maxDuration)
	default:
		return nil, 0, ErrUnsupported
	}
	if err != nil {
		return nil, 0, err
	}
	duration := time.Duration(len(pcm)) * time.Second / SampleRate
	if duration < MinDuration {
		return nil, 0, ErrTooShort
	}
	return encodeWAV(pcm), duration, nil
}

func isWAV(b []byte) bool {
	return len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE"
}

func isMP3(b []byte) bool {
	if len(b) >= 3 && string(b[0:3]) == "ID3" {
		return true
	}
	// フレームの同期ワード（11ビット）
	return len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0
}

// --- WAV

type wavFormat struct {
	format     uint16 // 1: PCM, 3: IEEE float
	channels   int
	sampleRate int
	bits       int
}

func convertWAV(data []byte, maxDuration time.Duration) ([]int16, error) {
	var f wavFormat
	var pcm []byte
	haveFmt := false
	for pos := 12; pos+8 <= len(data); {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := pos + 8
		end := body + size
		if size < 0 || end > len(data) {
			// 途中で切れているファイルは、data チャンクなら読めるところまで使う
			if id != "data" {
				return nil, ErrUnsupported
			}
			end = len(data)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, ErrUnsupported
			}
			c := data[body:end]
			f.format = binary.LittleEndian.Uint16(c[0:2])
			f.channels = int(binary.LittleEndian.Uint16(c[2:4]))
			f.sampleRate = int(binary.LittleEndian.Uint32(c[4:8]))
			f.bits = int(binary.LittleEndian.Uint16(c[14:16]))
			// WAVE_FORMAT_EXTENSIBLE は SubFormat の先頭2バイトが実際の形式
			if f.format == 0xFFFE && size >= 26 {
				f.format = binary.LittleEndian.Uint16(c[24:26])
			}
			haveFmt = true
		case "data":
			pcm = data[body:end]
		}
		pos = end + size%2 // チャンクは2バイト境界にそろう
	}
	if !haveFmt || pcm == nil || f.channels < 1 || f.channels > 8 || f.sampleRate < 4000 || f.sampleRate > 192000 {
		return nil, ErrUnsupported
	}
	var read func([]byte) float32
	switch {
	case f.format == 1 && f.bits == 8:
		read = func(b []byte) float32 { return (float32(b[0]) - 128) / 128 }
	case f.format == 1 && f.bits == 16:
		read = func(b []byte) float32 { return float32(int16(binary.LittleEndian.Uint16(b))) / 32768 }
	case f.format == 1 && f.bits == 24:
		read = func(b []byte) float32 {
			v := int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
			return float32(v) / 8388608
		}
	case f.format == 1 && f.bits == 32:
		read = func(b []byte) float32 { return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648 }
	case f.format == 3 && f.bits == 32:
		read = func(b []byte) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(b)) }
	default:
		return nil, ErrUnsupported
	}
	bytesPerSample := f.bits / 8
	frameSize := bytesPerSample * f.channels
	frames := len(pcm) / frameSize
	if time.Duration(frames)*time.Second/time.Duration(f.sampleRate) > maxDuration {
		return nil, ErrTooLong
	}
	r := newResampler(f.sampleRate, frames)
	for i := 0; i < frames; i++ {
		frame := pcm[i*frameSize : (i+1)*frameSize]
		var sum float32
		for c := 0; c < f.channels; c++ {
			sum += read(frame[c*bytesPerSample:])
		}
		r.push(sum / float32(f.channels))
	}
	return r.finish(), nil
}

// --- MP3

func convertMP3(data []byte, maxDuration time.Duration) ([]int16, error) {
	d, err := mp3.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, ErrUnsupported
	}
	rate := d.SampleRate()
	if rate < 4000 {
		return nil, ErrUnsupported
	}
	// 出力は 16bit・ステレオ（1フレーム4バイト）。Length は全体を走査して求める
	frames := int(d.Length() / 4)
	if time.Duration(frames)*time.Second/time.Duration(rate) > maxDuration {
		return nil, ErrTooLong
	}
	r := newResampler(rate, frames)
	buf := make([]byte, 4*4096)
	var carry []byte
	for {
		n, err := d.Read(buf)
		chunk := append(carry, buf[:n]...)
		usable := len(chunk) - len(chunk)%4
		for i := 0; i < usable; i += 4 {
			left := float32(int16(binary.LittleEndian.Uint16(chunk[i:])))
			right := float32(int16(binary.LittleEndian.Uint16(chunk[i+2:])))
			r.push((left + right) / 65536)
		}
		carry = append(carry[:0], chunk[usable:]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
	}
	return r.finish(), nil
}

// --- 標本化周波数の変換

// resampler は入力を 8kHz に変換する。窓関数付き sinc（ハミング窓）の低域通過フィルターを
// 出力の時刻ごとに評価する（多相表で分数の位置を近似する）。折り返し雑音が電話帯域に入らないよう、
// 遮断周波数は 3.3kHz（入力が 8kHz 未満なら入力のナイキスト周波数の 9 割）にする。
type resampler struct {
	ratio  float64 // 入力の標本数 / 出力の標本数
	half   int     // フィルターの片側の長さ
	phases int
	table  [][]float32 // [位相][2*half+1]

	buf  []float32 // 入力（buf[0] が入力の標本番号 base）
	base int
	next float64 // 次の出力の時刻（入力の標本番号）
	out  []float32
}

func newResampler(inRate, expectedFrames int) *resampler {
	cutoff := 3300.0
	if limit := 0.45 * float64(inRate); cutoff > limit {
		cutoff = limit
	}
	// ハミング窓の遷移帯域は約 3.3×fs/タップ数。約 1.2kHz になる長さにする
	half := int(math.Ceil(3.3 * float64(inRate) / 1200 / 2))
	if half < 16 {
		half = 16
	}
	if half > 100 {
		half = 100
	}
	const phases = 32
	fc := cutoff / float64(inRate) // 正規化した遮断周波数
	table := make([][]float32, phases)
	for p := 0; p < phases; p++ {
		row := make([]float32, 2*half+1)
		frac := float64(p) / phases
		var sum float64
		for k := -half; k <= half; k++ {
			x := float64(k) - frac
			v := 2 * fc
			if x != 0 {
				v = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
			}
			w := 0.54 + 0.46*math.Cos(math.Pi*x/float64(half+1))
			row[k+half] = float32(v * w)
			sum += v * w
		}
		// 直流の利得を 1 にそろえる
		for i := range row {
			row[i] = float32(float64(row[i]) / sum)
		}
		table[p] = row
	}
	return &resampler{
		ratio: float64(inRate) / SampleRate, half: half, phases: phases, table: table,
		out: make([]float32, 0, int(float64(expectedFrames)/(float64(inRate)/SampleRate))+1),
	}
}

func (r *resampler) push(x float32) {
	r.buf = append(r.buf, x)
	r.drain(false)
}

// drain は、必要な入力がそろった出力を計算する。
func (r *resampler) drain(final bool) {
	total := r.base + len(r.buf)
	for {
		n0 := int(math.Floor(r.next))
		frac := r.next - float64(n0)
		p := int(math.Round(frac * float64(r.phases)))
		if p == r.phases {
			n0++
			p = 0
		}
		if final {
			if n0 >= total {
				break
			}
		} else if n0+r.half >= total {
			break
		}
		row := r.table[p]
		var acc float32
		for k := -r.half; k <= r.half; k++ {
			i := n0 + k - r.base
			if i < 0 || i >= len(r.buf) {
				continue // 先頭と末尾は 0 とみなす
			}
			acc += r.buf[i] * row[k+r.half]
		}
		r.out = append(r.out, acc)
		r.next += r.ratio
	}
	// もう使わない入力を捨てる
	keepFrom := int(math.Floor(r.next)) - r.half - 1 - r.base
	if keepFrom > 8192 {
		r.buf = append(r.buf[:0], r.buf[keepFrom:]...)
		r.base += keepFrom
	}
}

func (r *resampler) finish() []int16 {
	r.drain(true)
	var peak float32
	for _, v := range r.out {
		if a := float32(math.Abs(float64(v))); a > peak {
			peak = a
		}
	}
	gain := float32(1)
	if peak > 0 {
		// -1dBFS にそろえる。ほぼ無音の音声を大きく持ち上げすぎない
		gain = 0.89 / peak
		if gain > 8 {
			gain = 8
		}
	}
	pcm := make([]int16, len(r.out))
	for i, v := range r.out {
		s := v * gain * 32767
		if s > 32767 {
			s = 32767
		} else if s < -32768 {
			s = -32768
		}
		pcm[i] = int16(s)
	}
	return pcm
}

// --- 出力

func encodeWAV(pcm []int16) []byte {
	dataSize := len(pcm) * 2
	b := make([]byte, 44+dataSize)
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], uint32(36+dataSize))
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	binary.LittleEndian.PutUint32(b[16:20], 16)
	binary.LittleEndian.PutUint16(b[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:24], 1) // モノラル
	binary.LittleEndian.PutUint32(b[24:28], SampleRate)
	binary.LittleEndian.PutUint32(b[28:32], SampleRate*2)
	binary.LittleEndian.PutUint16(b[32:34], 2)
	binary.LittleEndian.PutUint16(b[34:36], 16)
	copy(b[36:40], "data")
	binary.LittleEndian.PutUint32(b[40:44], uint32(dataSize))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[44+i*2:], uint16(v))
	}
	return b
}
