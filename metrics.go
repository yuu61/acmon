package main

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

type SoundcardSnapshot struct {
	THD            float64
	Harmonics      map[int]float64 // order -> 比率
	NoiseFloorDBFS float64
	FlickerPst     float64
	TransientTotal uint64
	SampleRate     float64
	Overruns       uint64
	LastSample     float64
	Valid          bool
}

// ----- ディスクリプタ -----
var (
	dVrms    = prometheus.NewDesc("acmon_voltage_rms_volts", "Line voltage RMS [V]", []string{"source"}, nil)
	dCrest   = prometheus.NewDesc("acmon_crest_factor", "Voltage crest factor (peak/RMS)", []string{"source"}, nil)
	dFreq    = prometheus.NewDesc("acmon_frequency_hertz", "Line frequency [Hz]", []string{"source"}, nil)
	dFreqDev = prometheus.NewDesc("acmon_frequency_deviation_hertz", "Frequency deviation from nominal [Hz]", []string{"source"}, nil)
	dTHD     = prometheus.NewDesc("acmon_thd_ratio", "Total harmonic distortion of voltage (ratio)", []string{"source"}, nil)
	dHarm    = prometheus.NewDesc("acmon_harmonic_ratio", "Per-order voltage harmonic ratio", []string{"source", "order"}, nil)
	dNoise   = prometheus.NewDesc("acmon_noise_floor_dbfs", "Spectral noise floor [dBFS]", []string{"source"}, nil)
	dPst     = prometheus.NewDesc("acmon_flicker_pst", "Simplified short-term flicker indicator (approx, not IEC-certified)", []string{"source"}, nil)

	dSag       = prometheus.NewDesc("acmon_sag_events_total", "Sag events detected", []string{"source"}, nil)
	dSwell     = prometheus.NewDesc("acmon_swell_events_total", "Swell events detected", []string{"source"}, nil)
	dTransient = prometheus.NewDesc("acmon_transient_events_total", "Transient/notch events detected", []string{"source"}, nil)

	dLastSample = prometheus.NewDesc("acmon_last_sample_timestamp_seconds", "Unix time of last completed analysis window", []string{"source"}, nil)
	dSampleRate = prometheus.NewDesc("acmon_sample_rate_hertz", "Achieved sample rate [Hz]", []string{"source"}, nil)
	dOverruns   = prometheus.NewDesc("acmon_buffer_overruns_total", "Sampling overruns / capture restarts", []string{"source"}, nil)
	dCPUTemp    = prometheus.NewDesc("acmon_cpu_temp_celsius", "SoC temperature [C]", nil, nil)
)

// Collector は2系統のスナップショットを emit する。
type Collector struct {
	zmpt      *atomic.Pointer[ZmptSnapshot]
	soundcard *atomic.Pointer[SoundcardSnapshot]
}

func NewCollector(z *atomic.Pointer[ZmptSnapshot], s *atomic.Pointer[SoundcardSnapshot]) *Collector {
	return &Collector{zmpt: z, soundcard: s}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
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

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	const srcZ = "zmpt"
	const srcS = "soundcard"

	if c.zmpt != nil {
		if z := c.zmpt.Load(); z != nil {
			if z.Valid {
				gauge(ch, dVrms, z.VoltageRMS, srcZ)
				gauge(ch, dCrest, z.CrestFactor, srcZ)
				gauge(ch, dFreq, z.FrequencyHz, srcZ) // 周波数は ZMPT を正とする
				gauge(ch, dFreqDev, z.FreqDeviation, srcZ)
				gauge(ch, dSampleRate, z.SampleRate, srcZ)
			}
			counter(ch, dSag, float64(z.SagTotal), srcZ)
			counter(ch, dSwell, float64(z.SwellTotal), srcZ)
			counter(ch, dOverruns, float64(z.Overruns), srcZ)
			gauge(ch, dLastSample, z.LastSample, srcZ)
		}
	}

	if c.soundcard != nil {
		if s := c.soundcard.Load(); s != nil {
			if s.Valid {
				gauge(ch, dTHD, s.THD, srcS)
				gauge(ch, dNoise, s.NoiseFloorDBFS, srcS)
				gauge(ch, dPst, s.FlickerPst, srcS)
				gauge(ch, dSampleRate, s.SampleRate, srcS)
				for ord, ratio := range s.Harmonics {
					gauge(ch, dHarm, ratio, srcS, strconv.Itoa(ord))
				}
			}
			counter(ch, dTransient, float64(s.TransientTotal), srcS)
			counter(ch, dOverruns, float64(s.Overruns), srcS)
			gauge(ch, dLastSample, s.LastSample, srcS)
		}
	}

	if t, ok := readCPUTemp(); ok {
		gauge(ch, dCPUTemp, t)
	}
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
