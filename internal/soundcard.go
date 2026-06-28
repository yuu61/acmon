package acmon

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"gonum.org/v1/gonum/dsp/fourier"
)

// 出力する高調波の次数（電圧高調波は奇数が主役）。
var harmonicOrders = []int{3, 5, 7, 9, 11, 13, 15}

// PCM フォーマットおよびスペクトル解析の領域定数。
const (
	formatS16      = "S16_LE"     // 16bit リトルエンディアン PCM
	minHistBlocks  = 8            // フリッカ評価に必要な最小ブロック数
	fundLowHz      = 55.0         // 基本波探索帯の下限[Hz]
	fundHighHz     = 65.0         // 基本波探索帯の上限[Hz]
	noiseLowCutHz  = 10.0         // ノイズ床推定で除外する低域カットオフ[Hz]
	dbfsRefScale   = 20.0         // 振幅→dBFS 変換係数 (20*log10)
	int16FullScale = 32768.0      // 2^15: S16 サンプルの正規化スケール
	int32FullScale = 2147483648.0 // 2^31: S32 サンプルの正規化スケール
)

// SoundcardSampler は arecord から生 PCM を読み、FFT で
// THD-V / 高調波 / ノイズ床 / 簡易フリッカ / 過渡を算出する。
type SoundcardSampler struct {
	cfg            *Config
	out            *atomic.Pointer[SoundcardSnapshot]
	fundHist       []float64
	transientTotal uint64
	overruns       uint64
	histCap        int
}

// NewSoundcardSampler はサウンドカード・スペクトルサンプラを生成する。
func NewSoundcardSampler(cfg *Config, out *atomic.Pointer[SoundcardSnapshot]) *SoundcardSampler {
	blockDur := float64(cfg.FFTSize) / float64(cfg.AudioRate)

	histCap := max(int(cfg.PstWindow.Seconds()/blockDur), minHistBlocks)

	return &SoundcardSampler{cfg: cfg, out: out, histCap: histCap}
}

func bytesPerSample(format string) int {
	switch format {
	case formatS16:
		return 2
	default: // S32_LE
		return 4
	}
}

// decode は1サンプル分のバイト列を [-1,1) に正規化する。
func decode(format string, b []byte) float64 {
	if format == formatS16 {
		v := int16(binary.LittleEndian.Uint16(b)) //nolint:gosec // 符号付きPCMへの意図的再解釈

		return float64(v) / int16FullScale
	}

	// S32_LE（24bit を上位詰めで載せるカードも同係数でOK）
	v := int32(binary.LittleEndian.Uint32(b)) //nolint:gosec // 符号付きPCMへの意図的再解釈

	return float64(v) / int32FullScale
}

// Run は arecord キャプチャを繰り返し、失敗時は 1 秒待って再起動する。ctx のキャンセルで終了する。
func (s *SoundcardSampler) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("soundcard run stopped: %w", ctx.Err())
		default:
		}

		err := s.captureOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("soundcard run stopped: %w", ctx.Err())
			}

			s.overruns++

			log.Printf("soundcard: capture error: %v (restarting in 1s)", err)
			time.Sleep(time.Second)
		}
	}
}

func (s *SoundcardSampler) captureOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "arecord", //nolint:gosec // 引数は flag 由来のオペレータ設定で外部入力ではない
		"-D", s.cfg.AudioDevice,
		"-c", "1",
		"-r", strconv.Itoa(s.cfg.AudioRate),
		"-f", s.cfg.AudioFormat,
		"-t", "raw",
		"-q",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("arecord stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("arecord start: %w", err)
	}

	defer func() {
		if werr := cmd.Wait(); werr != nil {
			log.Printf("soundcard: arecord exited: %v", werr)
		}
	}()

	bps := bytesPerSample(s.cfg.AudioFormat)
	n := s.cfg.FFTSize
	frame := make([]byte, n*bps)
	fft := fourier.NewFFT(n)
	win := hann(n)

	var winSum float64
	for _, w := range win {
		winSum += w
	}

	buf := make([]float64, n)
	log.Printf(
		"soundcard: started dev=%s rate=%d fmt=%s fft=%d",
		s.cfg.AudioDevice,
		s.cfg.AudioRate,
		s.cfg.AudioFormat,
		n,
	)

	for {
		if _, err := io.ReadFull(stdout, frame); err != nil {
			return fmt.Errorf("arecord read: %w", err)
		}

		for i := range n {
			buf[i] = decode(s.cfg.AudioFormat, frame[i*bps:(i+1)*bps])
		}

		s.analyze(buf, win, winSum, fft)
	}
}

func (s *SoundcardSampler) analyze(buf, win []float64, winSum float64, fft *fourier.FFT) {
	rate := float64(s.cfg.AudioRate)
	binHz := rate / float64(len(buf))
	dc := mean(buf)

	s.detectTransients(buf, dc)

	amp := amplitudeSpectrum(buf, win, winSum, dc, fft)
	fundBin := findFundamentalBin(amp, binHz)
	f0 := float64(fundBin) * binHz
	fundAmp := clusterAmp(amp, fundBin, 1)

	harm := harmonicRatios(amp, f0, binHz, fundAmp)
	thd := computeTHD(amp, f0, binHz, fundAmp)
	mask := buildHarmonicMask(amp, fundBin, f0, binHz)
	noiseDBFS := noiseFloorDBFS(amp, mask, binHz)
	pst := s.updateFlicker(fundAmp)

	snap := &SoundcardSnapshot{
		THD:            thd,
		Harmonics:      harm,
		NoiseFloorDBFS: noiseDBFS,
		FlickerPst:     pst,
		TransientTotal: s.transientTotal,
		SampleRate:     rate,
		Overruns:       s.overruns,
		LastSample:     float64(time.Now().UnixNano()) / 1e9,
		Valid:          true,
	}
	s.out.Store(snap)
}

// detectTransients は期待ピークの TransientK 倍を超えるサンプルを過渡として計数する。
// 1ms の不感時間（refractory）で連続検出を抑制する。
func (s *SoundcardSampler) detectTransients(buf []float64, dc float64) {
	var sq float64

	for _, v := range buf {
		d := v - dc
		sq += d * d
	}

	rmsT := math.Sqrt(sq / float64(len(buf)))
	expPeak := rmsT * math.Sqrt2
	thr := expPeak * s.cfg.TransientK
	refractory := s.cfg.AudioRate / 1000 // 1ms 不感

	cool := 0
	for _, v := range buf {
		if cool > 0 {
			cool--

			continue
		}

		if math.Abs(v-dc) > thr && thr > 0 {
			s.transientTotal++
			cool = refractory
		}
	}
}

// amplitudeSpectrum は DC 除去・窓掛けした信号の片側振幅スペクトルを返す。
func amplitudeSpectrum(buf, win []float64, winSum, dc float64, fft *fourier.FFT) []float64 {
	windowed := make([]float64, len(buf))
	for i := range buf {
		windowed[i] = (buf[i] - dc) * win[i]
	}

	return spectrum(fft, windowed, winSum)
}

// findFundamentalBin は基本波探索帯（fundLowHz..fundHighHz）で最大振幅の bin を返す。
func findFundamentalBin(amp []float64, binHz float64) int {
	loB := nearestBin(fundLowHz, binHz)
	hiB := nearestBin(fundHighHz, binHz)

	if loB < 1 {
		loB = 1
	}

	if hiB >= len(amp) {
		hiB = len(amp) - 1
	}

	fundBin := loB
	for k := loB; k <= hiB; k++ {
		if amp[k] > amp[fundBin] {
			fundBin = k
		}
	}

	return fundBin
}

// harmonicRatios は各次高調波の基本波に対する振幅比を返す。
func harmonicRatios(amp []float64, f0, binHz, fundAmp float64) map[int]float64 {
	harm := make(map[int]float64, len(harmonicOrders))
	for _, ord := range harmonicOrders {
		b := nearestBin(f0*float64(ord), binHz)
		if b >= len(amp) {
			harm[ord] = 0

			continue
		}

		if fundAmp > 0 {
			harm[ord] = clusterAmp(amp, b, 1) / fundAmp
		}
	}

	return harm
}

// computeTHD は 2..最大次（Nyquist 以下）の高調波パワー総和の平方根を基本波で割った THD-V を返す。
func computeTHD(amp []float64, f0, binHz, fundAmp float64) float64 {
	maxOrder := harmonicOrders[len(harmonicOrders)-1]

	var hsq float64

	for ord := 2; ord <= maxOrder; ord++ {
		b := nearestBin(f0*float64(ord), binHz)
		if b >= len(amp) {
			break
		}

		a := clusterAmp(amp, b, 1)
		hsq += a * a
	}

	if fundAmp <= 0 {
		return 0
	}

	return math.Sqrt(hsq) / fundAmp
}

// buildHarmonicMask は基本波・各高調波クラスタを真にしたマスクを返す（ノイズ床推定で除外する）。
func buildHarmonicMask(amp []float64, fundBin int, f0, binHz float64) []bool {
	maxOrder := harmonicOrders[len(harmonicOrders)-1]
	mask := make([]bool, len(amp))
	markCluster(mask, fundBin, 2)

	for ord := 2; ord <= maxOrder; ord++ {
		markCluster(mask, nearestBin(f0*float64(ord), binHz), 2)
	}

	return mask
}

// updateFlicker は基本波振幅の履歴を更新し、簡易フリッカ指標 Pst（近似）を返す。
func (s *SoundcardSampler) updateFlicker(fundAmp float64) float64 {
	s.fundHist = append(s.fundHist, fundAmp)
	if len(s.fundHist) > s.histCap {
		s.fundHist = s.fundHist[len(s.fundHist)-s.histCap:]
	}

	return pseudoPst(s.fundHist) * s.cfg.PstGain
}

func markCluster(mask []bool, center, span int) {
	for k := center - span; k <= center+span; k++ {
		if k >= 0 && k < len(mask) {
			mask[k] = true
		}
	}
}

// noiseFloorDBFS はマスク外 bin（DC 近傍と数 Hz 以下も除外）の中央値を dBFS で返す。
func noiseFloorDBFS(amp []float64, mask []bool, binHz float64) float64 {
	lowCut := int(math.Ceil(noiseLowCutHz / binHz)) // 低域は除外

	vals := make([]float64, 0, len(amp))
	for k := lowCut; k < len(amp); k++ {
		if !mask[k] {
			vals = append(vals, amp[k])
		}
	}

	if len(vals) == 0 {
		return -120
	}

	m := median(vals)
	if m <= 0 {
		return -120
	}

	return dbfsRefScale * math.Log10(m)
}

// pseudoPst は基本波振幅履歴の相対変動 RMS を返す（無次元）。
// 注意: IEC 61000-4-15 の正式 Pst ではない近似指標。
func pseudoPst(h []float64) float64 {
	if len(h) < 4 {
		return 0
	}

	m := mean(h)
	if m <= 0 {
		return 0
	}

	var sq float64

	for _, v := range h {
		r := (v - m) / m
		sq += r * r
	}

	return math.Sqrt(sq / float64(len(h)))
}

func median(x []float64) float64 {
	c := make([]float64, len(x))
	copy(c, x)
	slices.Sort(c)

	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}

	return (c[n/2-1] + c[n/2]) / 2
}
