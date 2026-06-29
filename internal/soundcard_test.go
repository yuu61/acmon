package acmon

import (
	"encoding/binary"
	"math"
	"testing"

	"gonum.org/v1/gonum/dsp/fourier"
)

func TestBytesPerSample(t *testing.T) {
	t.Parallel()

	cases := map[string]int{"S16_LE": 2, "S32_LE": 4, "unknown": 4}
	for format, want := range cases {
		if got := bytesPerSample(format); got != want {
			t.Errorf("bytesPerSample(%q) = %d, want %d", format, got, want)
		}
	}
}

func TestDecode(t *testing.T) {
	t.Parallel()

	var s16 int16 = 16384 // フルスケール 32768 の半分 → +0.5

	b16 := make([]byte, 2)
	binary.LittleEndian.PutUint16(b16, uint16(s16))
	approx(t, "decode S16 +0.5", decode("S16_LE", b16), 0.5, 1e-9)

	s16 = -32768                                    // 負側フルスケール → -1.0
	binary.LittleEndian.PutUint16(b16, uint16(s16)) //nolint:gosec // テスト用の符号付き値再解釈
	approx(t, "decode S16 -1.0", decode("S16_LE", b16), -1.0, 1e-9)

	var s32 int32 = 1 << 30 // フルスケール 2^31 の半分 → +0.5

	b32 := make([]byte, 4)
	binary.LittleEndian.PutUint32(b32, uint32(s32))
	approx(t, "decode S32 +0.5", decode("S32_LE", b32), 0.5, 1e-9)
}

func TestMedian(t *testing.T) {
	t.Parallel()
	approx(t, "median odd", median([]float64{3, 1, 2}), 2, 1e-9)
	approx(t, "median even", median([]float64{4, 1, 3, 2}), 2.5, 1e-9)

	// 入力スライスを破壊しないこと（内部でコピーする前提）。
	in := []float64{3, 1, 2}
	_ = median(in)

	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("median mutated input: %v", in)
	}
}

func TestPseudoPst(t *testing.T) {
	t.Parallel()
	approx(t, "pst short", pseudoPst([]float64{1, 1, 1}), 0, 1e-9) // len<4 → 0
	approx(t, "pst constant", pseudoPst([]float64{1, 1, 1, 1}), 0, 1e-9)
	// 平均 1.0、相対変動 ±0.1 → RMS = 0.1。
	approx(t, "pst variation", pseudoPst([]float64{0.9, 1.1, 0.9, 1.1}), 0.1, 1e-9)
}

// TestFindFundamental50Hz は 50Hz 系統（東日本）でも基本波ビンを正しく拾うことを確認する。
// fundLowHz を 55→45Hz に広げた回帰テスト。旧 55Hz 床では bin10(=50Hz) が探索帯から外れていた。
func TestFindFundamental50Hz(t *testing.T) {
	t.Parallel()

	const (
		rate  = 48000.0
		n     = 9600
		binHz = rate / n // 5.0 Hz/bin
		fund  = 50.0     // ちょうど bin 10
		want  = 10
	)

	win := hann(n)

	var winSum float64
	for _, w := range win {
		winSum += w
	}

	buf := sine(n, fund, rate)

	amp := amplitudeSpectrum(buf, win, winSum, mean(buf), fourier.NewFFT(n))
	if got := findFundamentalBin(amp, binHz); got != want {
		t.Errorf("findFundamentalBin(50Hz) = %d, want %d", got, want)
	}
}

// TestSpectralHarmonics は基本波 + 既知の 3 次/5 次高調波を持つ合成信号を流し、
// 基本波ビン検出・高調波比・THD が理論値に一致することを確認する（PQ 計算の数学検証）。
func TestSpectralHarmonics(t *testing.T) {
	t.Parallel()

	const (
		rate     = 48000.0
		n        = 8000
		binHz    = rate / n // 6.0 Hz/bin
		fund     = 60.0     // bin 10
		h3Ratio  = 0.05     // 3 次 = 基本波の 5%
		h5Ratio  = 0.10     // 5 次 = 基本波の 10%
		wantFund = 10
	)

	buf := make([]float64, n)
	for i := range buf {
		buf[i] = math.Sin(2*math.Pi*fund*float64(i)/rate) +
			h3Ratio*math.Sin(2*math.Pi*3*fund*float64(i)/rate) +
			h5Ratio*math.Sin(2*math.Pi*5*fund*float64(i)/rate)
	}

	win := hann(n)

	var winSum float64
	for _, w := range win {
		winSum += w
	}

	amp := amplitudeSpectrum(buf, win, winSum, mean(buf), fourier.NewFFT(n))

	fundBin := findFundamentalBin(amp, binHz)
	if fundBin != wantFund {
		t.Fatalf("findFundamentalBin = %d, want %d", fundBin, wantFund)
	}

	f0 := float64(fundBin) * binHz
	fundAmp := clusterAmp(amp, fundBin)

	harm := harmonicRatios(amp, f0, binHz, fundAmp)
	approx(t, "harmonic[3]", harm[3], h3Ratio, 0.01)
	approx(t, "harmonic[5]", harm[5], h5Ratio, 0.01)
	approx(t, "harmonic[7]", harm[7], 0, 0.01)

	wantTHD := math.Sqrt(h3Ratio*h3Ratio + h5Ratio*h5Ratio)
	approx(t, "THD", computeTHD(amp, f0, binHz, fundAmp), wantTHD, 0.01)
}
