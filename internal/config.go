// Package acmon は ZMPT101B+MCP3208 電圧フロントエンドと USB サウンドカードの
// スペクトル・フロントエンドから電力品質メトリクスを収集し、Prometheus 形式で公開する。
package acmon

import (
	"flag"
	"time"
)

// Config は実行時設定。flag で上書きする。
type Config struct {
	Listen               string
	AudioFormat          string
	SPIPort              string
	AudioDevice          string
	SagVolts             float64
	FFTSize              int
	ZmptWindow           time.Duration
	ZmptCalVoltsPerCount float64
	ADCChannel           int
	SwellVolts           float64
	EventHystV           float64
	PstGain              float64
	SPIHz                int
	AudioRate            int
	PstWindow            time.Duration
	ZmptRate             int
	TransientK           float64
	EnableZMPT           bool
	EnableSoundcard      bool
}

// ParseFlags はコマンドライン引数を解釈し、確定した Config を返す。起動時に main から一度だけ呼ぶ。
func ParseFlags() *Config {
	c := &Config{}
	flag.StringVar(&c.Listen, "listen", ":9100", "HTTP listen address for /metrics")

	flag.BoolVar(&c.EnableZMPT, "enable-zmpt", true, "enable ZMPT101B+MCP3208 voltage front-end")
	flag.StringVar(&c.SPIPort, "spi-port", "", "SPI port (empty=first, or /dev/spidev0.0)")
	flag.IntVar(&c.SPIHz, "spi-hz", 1_000_000, "SPI clock [Hz]")
	flag.IntVar(&c.ADCChannel, "adc-channel", 0, "MCP3208 channel 0..7")
	flag.IntVar(&c.ZmptRate, "zmpt-rate", 8000, "ZMPT target sample rate [SPS]")

	windowMs := flag.Int("zmpt-window-ms", 200, "ZMPT analysis window [ms]")

	flag.Float64Var(
		&c.ZmptCalVoltsPerCount,
		"zmpt-cal",
		0.0,
		"calibration: line volts per RMS ADC count (must be set, see README)",
	)
	flag.Float64Var(&c.SagVolts, "sag-volts", 95.0, "sag threshold [V]")
	flag.Float64Var(&c.SwellVolts, "swell-volts", 107.0, "swell threshold [V]")
	flag.Float64Var(&c.EventHystV, "event-hyst", 1.0, "sag/swell recovery hysteresis [V]")

	flag.BoolVar(&c.EnableSoundcard, "enable-soundcard", true, "enable USB-soundcard spectral front-end")
	flag.StringVar(&c.AudioDevice, "audio-device", "default", "arecord -D device")
	flag.IntVar(&c.AudioRate, "audio-rate", 48000, "audio sample rate [Hz]")
	flag.StringVar(&c.AudioFormat, "audio-format", "S32_LE", "audio format: S32_LE|S16_LE")
	flag.IntVar(&c.FFTSize, "fft-size", 8192, "FFT size (power of two)")
	flag.Float64Var(&c.TransientK, "transient-k", 1.6, "transient threshold as multiple of expected peak")

	pstMs := flag.Int("pst-window-ms", 10000, "flicker (Pst) evaluation window [ms]")

	flag.Float64Var(&c.PstGain, "pst-gain", 50.0, "scale factor for simplified flicker indicator")

	flag.Parse() //nolint:revive // deep-exit: 設定集約関数なので main 同様に flag.Parse をここで呼ぶ

	c.ZmptWindow = time.Duration(*windowMs) * time.Millisecond
	c.PstWindow = time.Duration(*pstMs) * time.Millisecond

	return c
}
