package acmon

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"gonum.org/v1/gonum/dsp/fourier"
)

// 出力する高調波の次数（電圧高調波は奇数が主役）。
var harmonicOrders = []int{3, 5, 7, 9, 11, 13, 15}

// SoundcardSampler は arecord から生 PCM を読み、FFT で
// THD-V / 高調波 / ノイズ床 / 簡易フリッカ / 過渡を算出する。
type SoundcardSampler struct {
	cfg *Config
	out *atomic.Pointer[SoundcardSnapshot]

	transientTotal uint64
	overruns       uint64

	// フリッカ評価用: 各ブロックの基本波振幅履歴
	fundHist []float64
	histCap  int
}

func NewSoundcardSampler(cfg *Config, out *atomic.Pointer[SoundcardSnapshot]) *SoundcardSampler {
	blockDur := float64(cfg.FFTSize) / float64(cfg.AudioRate)
	histCap := int(cfg.PstWindow.Seconds() / blockDur)
	if histCap < 8 {
		histCap = 8
	}
	return &SoundcardSampler{cfg: cfg, out: out, histCap: histCap}
}

func bytesPerSample(format string) int {
	switch format {
	case "S16_LE":
		return 2
	default: // S32_LE
		return 4
	}
}

// decode は1サンプル分のバイト列を [-1,1) に正規化する。
func decode(format string, b []byte) float64 {
	switch format {
	case "S16_LE":
		return float64(int16(binary.LittleEndian.Uint16(b))) / 32768.0
	default: // S32_LE（24bit を上位詰めで載せるカードも同係数でOK）
		return float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648.0
	}
}

func (s *SoundcardSampler) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := s.captureOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.overruns++
			log.Printf("soundcard: capture error: %v (restarting in 1s)", err)
			time.Sleep(time.Second)
		}
	}
}

func (s *SoundcardSampler) captureOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "arecord",
		"-D", s.cfg.AudioDevice,
		"-c", "1",
		"-r", strconv.Itoa(s.cfg.AudioRate),
		"-f", s.cfg.AudioFormat,
		"-t", "raw",
		"-q",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()

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
	log.Printf("soundcard: started dev=%s rate=%d fmt=%s fft=%d", s.cfg.AudioDevice, s.cfg.AudioRate, s.cfg.AudioFormat, n)

	for {
		if _, err := io.ReadFull(stdout, frame); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			buf[i] = decode(s.cfg.AudioFormat, frame[i*bps:(i+1)*bps])
		}
		s.analyze(buf, win, winSum, fft)
	}
}

func (s *SoundcardSampler) analyze(buf, win []float64, winSum float64, fft *fourier.FFT) {
	n := len(buf)
	rate := float64(s.cfg.AudioRate)
	binHz := rate / float64(n)

	// --- 時間領域: 過渡検出 ---
	dc := mean(buf)
	var sq float64
	for _, v := range buf {
		d := v - dc
		sq += d * d
	}
	rmsT := math.Sqrt(sq / float64(n))
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

	// --- 周波数領域 ---
	windowed := make([]float64, n)
	for i := range buf {
		windowed[i] = (buf[i] - dc) * win[i]
	}
	amp, _, _ := spectrum(fft, windowed, winSum)

	// 基本波探索（55..65Hz の最大 bin）
	loB := nearestBin(55, binHz)
	hiB := nearestBin(65, binHz)
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
	f0 := float64(fundBin) * binHz
	fundAmp := clusterAmp(amp, fundBin, 1)

	// 高調波抽出
	harm := make(map[int]float64, len(harmonicOrders))
	maxOrder := harmonicOrders[len(harmonicOrders)-1]
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

	// THD-V: 2..maxOrder（Nyquist 以下）の高調波パワー総和 / 基本波
	var hsq float64
	for ord := 2; ord <= maxOrder; ord++ {
		b := nearestBin(f0*float64(ord), binHz)
		if b >= len(amp) {
			break
		}
		a := clusterAmp(amp, b, 1)
		hsq += a * a
	}
	thd := 0.0
	if fundAmp > 0 {
		thd = math.Sqrt(hsq) / fundAmp
	}

	// ノイズ床: 基本波・高調波クラスタを除いた bin の中央値振幅 → dBFS
	mask := make([]bool, len(amp))
	markCluster(mask, fundBin, 2)
	for ord := 2; ord <= maxOrder; ord++ {
		markCluster(mask, nearestBin(f0*float64(ord), binHz), 2)
	}
	noiseDBFS := noiseFloorDBFS(amp, mask, binHz)

	// 簡易フリッカ: 基本波振幅の相対変動を蓄積し標準偏差 * gain
	s.fundHist = append(s.fundHist, fundAmp)
	if len(s.fundHist) > s.histCap {
		s.fundHist = s.fundHist[len(s.fundHist)-s.histCap:]
	}
	pst := pseudoPst(s.fundHist) * s.cfg.PstGain

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

func markCluster(mask []bool, center, span int) {
	for k := center - span; k <= center+span; k++ {
		if k >= 0 && k < len(mask) {
			mask[k] = true
		}
	}
}

// noiseFloorDBFS はマスク外 bin（DC 近傍と数 Hz 以下も除外）の中央値を dBFS で返す。
func noiseFloorDBFS(amp []float64, mask []bool, binHz float64) float64 {
	lowCut := int(math.Ceil(10.0 / binHz)) // 10Hz 以下は除外
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
	return 20 * math.Log10(m)
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
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}
