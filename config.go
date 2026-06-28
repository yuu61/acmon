package main

import (
	"flag"
	"time"
)

// Config は実行時設定。flag で上書きする。
type Config struct {
	Listen string

	// ----- ZMPT101B + MCP3208 系統 -----
	EnableZMPT  bool
	SPIPort     string  // "" で先頭、または "/dev/spidev0.0"
	SPIHz       int     // SPI クロック
	ADCChannel  int     // MCP3208 のチャンネル 0..7
	ZmptRate    int     // 目標サンプルレート [SPS]
	ZmptWindow  time.Duration
	ADCBits     int     // MCP3208 = 12bit
	// 校正: RMS カウント → 系統電圧[V] の係数。要実測校正（README 参照）。
	ZmptCalVoltsPerCount float64
	NominalVolts float64
	SagVolts     float64 // この値を下回ったらサグ
	SwellVolts   float64 // この値を上回ったらスウェル
	EventHystV   float64 // 復帰ヒステリシス[V]

	// ----- USB サウンドカード系統 -----
	EnableSoundcard bool
	AudioDevice     string // arecord -D に渡す（例 "hw:1,0" / "default"）
	AudioRate       int
	AudioFormat     string // "S32_LE" | "S16_LE"
	FFTSize         int    // 2 のべき乗推奨
	TransientK      float64 // 期待ピークの何倍を過渡とみなすか
	PstWindow       time.Duration // フリッカ評価窓
	PstGain         float64 // 簡易 Pst のスケール係数（無次元、要現場調整）
}

func parseFlags() *Config {
	c := &Config{}
	flag.StringVar(&c.Listen, "listen", ":9100", "HTTP listen address for /metrics")

	flag.BoolVar(&c.EnableZMPT, "enable-zmpt", true, "enable ZMPT101B+MCP3208 voltage front-end")
	flag.StringVar(&c.SPIPort, "spi-port", "", "SPI port (empty=first, or /dev/spidev0.0)")
	flag.IntVar(&c.SPIHz, "spi-hz", 1_000_000, "SPI clock [Hz]")
	flag.IntVar(&c.ADCChannel, "adc-channel", 0, "MCP3208 channel 0..7")
	flag.IntVar(&c.ZmptRate, "zmpt-rate", 8000, "ZMPT target sample rate [SPS]")
	windowMs := flag.Int("zmpt-window-ms", 200, "ZMPT analysis window [ms]")
	flag.Float64Var(&c.ZmptCalVoltsPerCount, "zmpt-cal", 0.0, "calibration: line volts per RMS ADC count (must be set, see README)")
	flag.Float64Var(&c.NominalVolts, "nominal-volts", 100.0, "nominal line voltage [V]")
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

	flag.Parse()

	c.ZmptWindow = time.Duration(*windowMs) * time.Millisecond
	c.PstWindow = time.Duration(*pstMs) * time.Millisecond
	c.ADCBits = 12
	return c
}
