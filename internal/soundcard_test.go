package acmon

import (
	"encoding/binary"
	"math"
	"sync/atomic"
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

// TestAnalyzeWiring は合成信号を analyze() に実際に通し、Snapshot の各フィールドが
// 正しい解析値へ結線されていることを検証する。フィールドの取り違えは個別 UT を全て
// 緑のまま通過しうるため、パイプライン再実装ではなく実配線を一度通して確認する。
func TestAnalyzeWiring(t *testing.T) {
	t.Parallel()

	const (
		rate    = 48000.0
		n       = 8000
		fund    = 60.0
		h3Ratio = 0.05
		h5Ratio = 0.10
	)

	buf := sine(n, fund, rate)
	h3 := sine(n, 3*fund, rate)
	h5 := sine(n, 5*fund, rate)

	for i := range buf {
		buf[i] += h3Ratio*h3[i] + h5Ratio*h5[i]
	}

	win := hann(n)

	var winSum float64
	for _, w := range win {
		winSum += w
	}

	cfg := &Config{AudioRate: rate, FFTSize: n, TransientK: 1.6, PstGain: 50.0}

	var out atomic.Pointer[SoundcardSnapshot]

	s := NewSoundcardSampler(cfg, &out)
	s.analyze(buf, win, winSum, fourier.NewFFT(n))

	snap := out.Load()
	if snap == nil {
		t.Fatal("analyze did not store a snapshot")
	}

	if !snap.Valid {
		t.Error("snapshot not marked Valid")
	}

	wantTHD := math.Sqrt(h3Ratio*h3Ratio + h5Ratio*h5Ratio)
	approx(t, "THD", snap.THD, wantTHD, 0.01)
	approx(t, "Harmonics[3]", snap.Harmonics[3], h3Ratio, 0.01)
	approx(t, "Harmonics[5]", snap.Harmonics[5], h5Ratio, 0.01)
	approx(t, "SampleRate", snap.SampleRate, rate, 1e-9)

	// should-be-zero フィールドへの誤結線も検出する: 単発呼び出し→履歴長1→Pst=0、過渡/overrun無し。
	approx(t, "FlickerPst", snap.FlickerPst, 0, 1e-9)

	if snap.Overruns != 0 {
		t.Errorf("Overruns = %d, want 0", snap.Overruns)
	}
}

// TestDetectTransients は 1ms 不感(refractory)の境界挙動を検証する。
// 同振幅スパイク2発が refractory 内なら2発目を抑制(計数1)、refractory 超なら両方計数(2)。
func TestDetectTransients(t *testing.T) {
	t.Parallel()

	const rate = 48000 // refractory = rate/1000 = 48 サンプル

	mkBuf := func(spikes ...int) []float64 {
		b := make([]float64, 1000)
		for _, p := range spikes {
			b[p] = 1.0
		}

		return b
	}

	cfg := &Config{AudioRate: rate, TransientK: 1.6}

	far := &SoundcardSampler{cfg: cfg}
	far.detectTransients(mkBuf(100, 200), 0) // 間隔100 > 48 → 2発とも計数

	if far.transientTotal != 2 {
		t.Errorf("far spikes: got %d, want 2", far.transientTotal)
	}

	near := &SoundcardSampler{cfg: cfg}
	near.detectTransients(mkBuf(100, 120), 0) // 間隔20 ≤ 48 → 2発目を refractory で抑制

	if near.transientTotal != 1 {
		t.Errorf("near spikes: got %d, want 1", near.transientTotal)
	}
}

// TestUpdateFlicker は履歴の histCap 切り詰め(最新 histCap 件のみ保持)と
// PstGain の乗算結線を検証する。
func TestUpdateFlicker(t *testing.T) {
	t.Parallel()

	// histCap=4 に対し6件投入 → 直近4件 [3,4,5,6] のみ残る。
	s := &SoundcardSampler{cfg: &Config{PstGain: 1.0}, histCap: 4}
	for _, v := range []float64{1, 2, 3, 4, 5, 6} {
		s.updateFlicker(v)
	}

	if len(s.fundHist) != 4 {
		t.Fatalf("histCap: len=%d, want 4", len(s.fundHist))
	}

	for i, want := range []float64{3, 4, 5, 6} {
		approx(t, "fundHist", s.fundHist[i], want, 1e-9)
	}

	// pseudoPst([0.9,1.1,0.9,1.1]) = 0.1、× PstGain(50) = 5.0（乗算が掛かっていることを確認）。
	g := &SoundcardSampler{cfg: &Config{PstGain: 50.0}, histCap: 8}

	var pst float64
	for _, v := range []float64{0.9, 1.1, 0.9, 1.1} {
		pst = g.updateFlicker(v)
	}

	approx(t, "flicker gain", pst, 5.0, 1e-9)
}

// TestNoiseFloorDBFS はマスク外 bin の中央値 dBFS 変換と、候補ゼロ／中央値ゼロ時の
// -120 dBFS フォールバックを検証する(lowCut = ceil(noiseLowCutHz/binHz))。
func TestNoiseFloorDBFS(t *testing.T) {
	t.Parallel()

	const binHz = 6.0 // lowCut = ceil(10/6) = 2 → bin0,1 は低域カットで除外

	// 通常: マスク外(bin2..4)の中央値 0.1 → 20*log10(0.1) = -20 dBFS。先頭2 bin はカット対象。
	amp := []float64{9, 9, 0.1, 0.1, 0.1}
	approx(t, "normal", noiseFloorDBFS(amp, make([]bool, len(amp)), binHz), -20, 1e-9)

	// 全 bin マスク → 候補ゼロ → -120 フォールバック。
	masked := make([]bool, len(amp))
	for i := range masked {
		masked[i] = true
	}

	approx(t, "all-masked", noiseFloorDBFS(amp, masked, binHz), -120, 1e-9)

	// マスク外がすべて 0 → median 0 → -120 フォールバック。
	approx(t, "zero-median", noiseFloorDBFS(make([]float64, 5), make([]bool, 5), binHz), -120, 1e-9)
}
