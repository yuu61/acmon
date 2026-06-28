package acmon

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// ----- スナップショット -----
// 各サンプラが窓ごとに最新値をまとめて atomic.Pointer で差し替える。
// scrape 側はこれを読むだけ（ロックフリー）。

// sourceLabel は各メトリクスに付与する系統識別ラベルの名前。
const sourceLabel = "source"

// ZmptSnapshot は ZMPT101B 系統の最新解析結果。サンプラが窓ごとに atomic に差し替える。
type ZmptSnapshot struct {
	VoltageRMS    float64
	CrestFactor   float64
	FrequencyHz   float64
	FreqDeviation float64
	SagTotal      uint64
	SwellTotal    uint64
	SampleRate    float64 // 実測達成レート
	Overruns      uint64
	LastSample    float64 // unix 秒
	Valid         bool
}

// SoundcardSnapshot はサウンドカード系統の最新スペクトル解析結果。
type SoundcardSnapshot struct {
	Harmonics      map[int]float64
	THD            float64
	NoiseFloorDBFS float64
	FlickerPst     float64
	TransientTotal uint64
	SampleRate     float64
	Overruns       uint64
	LastSample     float64
	Valid          bool
}

// ----- ディスクリプタ -----.
var (
	dVrms    = prometheus.NewDesc("acmon_voltage_rms_volts", "Line voltage RMS [V]", []string{sourceLabel}, nil)
	dCrest   = prometheus.NewDesc("acmon_crest_factor", "Voltage crest factor (peak/RMS)", []string{sourceLabel}, nil)
	dFreq    = prometheus.NewDesc("acmon_frequency_hertz", "Line frequency [Hz]", []string{sourceLabel}, nil)
	dFreqDev = prometheus.NewDesc(
		"acmon_frequency_deviation_hertz",
		"Frequency deviation from nominal [Hz]",
		[]string{sourceLabel},
		nil,
	)
	dTHD = prometheus.NewDesc(
		"acmon_thd_ratio",
		"Total harmonic distortion of voltage (ratio)",
		[]string{sourceLabel},
		nil,
	)
	dHarm = prometheus.NewDesc(
		"acmon_harmonic_ratio",
		"Per-order voltage harmonic ratio",
		[]string{sourceLabel, "order"},
		nil,
	)
	dNoise = prometheus.NewDesc("acmon_noise_floor_dbfs", "Spectral noise floor [dBFS]", []string{sourceLabel}, nil)
	dPst   = prometheus.NewDesc(
		"acmon_flicker_pst",
		"Simplified short-term flicker indicator (approx, not IEC-certified)",
		[]string{sourceLabel},
		nil,
	)

	dSag       = prometheus.NewDesc("acmon_sag_events_total", "Sag events detected", []string{sourceLabel}, nil)
	dSwell     = prometheus.NewDesc("acmon_swell_events_total", "Swell events detected", []string{sourceLabel}, nil)
	dTransient = prometheus.NewDesc(
		"acmon_transient_events_total",
		"Transient/notch events detected",
		[]string{sourceLabel},
		nil,
	)

	dLastSample = prometheus.NewDesc(
		"acmon_last_sample_timestamp_seconds",
		"Unix time of last completed analysis window",
		[]string{sourceLabel},
		nil,
	)
	dSampleRate = prometheus.NewDesc("acmon_sample_rate_hertz", "Achieved sample rate [Hz]", []string{sourceLabel}, nil)
	dOverruns   = prometheus.NewDesc(
		"acmon_buffer_overruns_total",
		"Sampling overruns / capture restarts",
		[]string{sourceLabel},
		nil,
	)
	dCPUTemp = prometheus.NewDesc("acmon_cpu_temp_celsius", "SoC temperature [C]", nil, nil)
)

// Collector は2系統のスナップショットを emit する。
type Collector struct {
	zmpt      *atomic.Pointer[ZmptSnapshot]
	soundcard *atomic.Pointer[SoundcardSnapshot]
}

// NewCollector は2系統のスナップショットポインタを束ねた Collector を返す。
func NewCollector(z *atomic.Pointer[ZmptSnapshot], s *atomic.Pointer[SoundcardSnapshot]) *Collector {
	return &Collector{zmpt: z, soundcard: s}
}

// Describe は公開する全メトリクスのディスクリプタを送出する（prometheus.Collector 実装）。
func (*Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- dVrms

	ch <- dCrest

	ch <- dFreq

	ch <- dFreqDev

	ch <- dTHD

	ch <- dHarm

	ch <- dNoise

	ch <- dPst

	ch <- dSag

	ch <- dSwell

	ch <- dTransient

	ch <- dLastSample

	ch <- dSampleRate

	ch <- dOverruns

	ch <- dCPUTemp
}

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func counter(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
}

// Collect は両系統の最新スナップショットと SoC 温度を送出する（prometheus.Collector 実装）。
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.collectZMPT(ch)
	c.collectSoundcard(ch)

	if t, ok := readCPUTemp(); ok {
		gauge(ch, dCPUTemp, t)
	}
}

func (c *Collector) collectZMPT(ch chan<- prometheus.Metric) {
	const src = "zmpt"

	if c.zmpt == nil {
		return
	}

	z := c.zmpt.Load()
	if z == nil {
		return
	}

	if z.Valid {
		gauge(ch, dVrms, z.VoltageRMS, src)
		gauge(ch, dCrest, z.CrestFactor, src)
		gauge(ch, dFreq, z.FrequencyHz, src) // 周波数は ZMPT を正とする
		gauge(ch, dFreqDev, z.FreqDeviation, src)
		gauge(ch, dSampleRate, z.SampleRate, src)
	}

	counter(ch, dSag, float64(z.SagTotal), src)
	counter(ch, dSwell, float64(z.SwellTotal), src)
	counter(ch, dOverruns, float64(z.Overruns), src)
	gauge(ch, dLastSample, z.LastSample, src)
}

func (c *Collector) collectSoundcard(ch chan<- prometheus.Metric) {
	const src = "soundcard"

	if c.soundcard == nil {
		return
	}

	s := c.soundcard.Load()
	if s == nil {
		return
	}

	if s.Valid {
		gauge(ch, dTHD, s.THD, src)
		gauge(ch, dNoise, s.NoiseFloorDBFS, src)
		gauge(ch, dPst, s.FlickerPst, src)
		gauge(ch, dSampleRate, s.SampleRate, src)

		for ord, ratio := range s.Harmonics {
			gauge(ch, dHarm, ratio, src, strconv.Itoa(ord))
		}
	}

	counter(ch, dTransient, float64(s.TransientTotal), src)
	counter(ch, dOverruns, float64(s.Overruns), src)
	gauge(ch, dLastSample, s.LastSample, src)
}

// readCPUTemp は SoC 温度を読む。取得不可なら ok=false。
func readCPUTemp() (float64, bool) {
	b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0, false
	}

	milli, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}

	return float64(milli) / 1000.0, true
}
