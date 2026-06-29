package acmon

import (
	"math"
	"testing"

	"gonum.org/v1/gonum/dsp/fourier"
)

// approx は got が want から tol 以内にあることを表明する共通ヘルパ。
func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()

	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %v, want %v (tol %v)", name, got, want, tol)
	}
}

// sine は振幅 1・周波数 freq[Hz] の正弦波を rate[SPS] で n サンプル生成する。
func sine(n int, freq, rate float64) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = math.Sin(2 * math.Pi * freq * float64(i) / rate)
	}

	return s
}

func TestMean(t *testing.T) {
	t.Parallel()
	approx(t, "mean", mean([]float64{1, 2, 3}), 2, 1e-9)
	approx(t, "mean empty", mean(nil), 0, 1e-9)
}

func TestRemoveDC(t *testing.T) {
	t.Parallel()

	got := removeDC([]float64{1, 2, 3})
	want := []float64{-1, 0, 1}

	for i := range want {
		approx(t, "removeDC", got[i], want[i], 1e-9)
	}
}

func TestRMSValue(t *testing.T) {
	t.Parallel()
	approx(t, "rms empty", rmsValue(nil), 0, 1e-9)

	// 振幅 1 の正弦波（整数周期）の RMS は 1/sqrt(2)。
	const rate = 8000.0

	s := sine(8000, 50, rate) // 1 秒 = 50 周期ちょうど
	approx(t, "rms sine", rmsValue(s), 1/math.Sqrt2, 1e-9)
}

func TestMaxAbs(t *testing.T) {
	t.Parallel()
	approx(t, "maxAbs", maxAbs([]float64{-3, 2, 1.5}), 3, 1e-9)
	approx(t, "maxAbs empty", maxAbs(nil), 0, 1e-9)
}

func TestZeroCrossFreq(t *testing.T) {
	t.Parallel()

	const (
		rate = 8000.0
		freq = 60.0
		n    = 2000 // 0.25 秒 = 15 周期
	)

	s := sine(n, freq, rate)

	tsec := make([]float64, n)
	for i := range tsec {
		tsec[i] = float64(i) / rate
	}

	approx(t, "zeroCrossFreq 60Hz", zeroCrossFreq(s, tsec), freq, 0.2)

	// 異常系: 長さ不一致・短すぎる入力は 0。
	approx(t, "len mismatch", zeroCrossFreq([]float64{1, -1, 1}, []float64{0}), 0, 1e-9)
	approx(t, "too short", zeroCrossFreq([]float64{1}, []float64{0}), 0, 1e-9)
}

func TestHann(t *testing.T) {
	t.Parallel()

	if w := hann(1); len(w) != 1 || w[0] != 1 {
		t.Fatalf("hann(1) = %v, want [1]", w)
	}

	const n = 16

	w := hann(n)

	approx(t, "hann start", w[0], 0, 1e-9)
	approx(t, "hann end", w[n-1], 0, 1e-9)

	for i, v := range w {
		if v < 0 || v > 1 {
			t.Errorf("hann[%d] = %v, out of [0,1]", i, v)
		}

		approx(t, "hann symmetry", v, w[n-1-i], 1e-9)
	}
}

func TestNearestBin(t *testing.T) {
	t.Parallel()

	if got := nearestBin(60, 6); got != 10 {
		t.Errorf("nearestBin(60,6) = %d, want 10", got)
	}

	if got := nearestBin(58, 6); got != 10 { // 9.67 → 10
		t.Errorf("nearestBin(58,6) = %d, want 10", got)
	}

	if got := nearestBin(0, 6); got != 0 {
		t.Errorf("nearestBin(0,6) = %d, want 0", got)
	}
}

func TestClusterAmp(t *testing.T) {
	t.Parallel()
	// 3-4-5: sqrt(3^2 + 4^2) のクラスタ合算。
	approx(t, "clusterAmp", clusterAmp([]float64{0, 3, 4, 0}, 1), 5, 1e-9)
	// 左端クランプ: center=0 → bin0,bin1 のみ。
	approx(t, "clusterAmp clamp", clusterAmp([]float64{0, 3, 4, 0}, 0), 3, 1e-9)
}

// TestSpectrumFullScale はフルスケール正弦波（振幅 1.0）が
// オンビン時に振幅スペクトルでほぼ 1.0（= 0 dBFS）になることを確認する。
func TestSpectrumFullScale(t *testing.T) {
	t.Parallel()

	const (
		rate = 48000.0
		n    = 8000
		k0   = 60 // 60 ビン = 360Hz（オンビン）
	)

	buf := sine(n, float64(k0)*rate/float64(n), rate)
	win := hann(n)

	var winSum float64
	for _, w := range win {
		winSum += w
	}

	amp := amplitudeSpectrum(buf, win, winSum, mean(buf), fourier.NewFFT(n))
	approx(t, "spectrum center bin (0 dBFS)", amp[k0], 1.0, 0.02)
}
