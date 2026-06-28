package acmon

import (
	"math"
	"math/cmplx"

	"gonum.org/v1/gonum/dsp/fourier"
)

// mean は平均を返す。
func mean(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var s float64
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

// removeDC は DC 成分（窓内平均）を引いた信号と、その DC 値を返す。
func removeDC(raw []float64) (s []float64, dc float64) {
	dc = mean(raw)
	s = make([]float64, len(raw))
	for i, v := range raw {
		s[i] = v - dc
	}
	return s, dc
}

// rmsValue は RMS を返す（DC 除去済みの信号を渡す前提）。
func rmsValue(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	var acc float64
	for _, v := range s {
		acc += v * v
	}
	return math.Sqrt(acc / float64(len(s)))
}

// maxAbs は絶対値の最大（波高値）を返す。
func maxAbs(s []float64) float64 {
	var m float64
	for _, v := range s {
		if a := math.Abs(v); a > m {
			m = a
		}
	}
	return m
}

// zeroCrossFreq は立ち上がりゼロクロスのタイムスタンプを線形補間して周波数を出す。
// s は DC 除去済み信号、tsec は各サンプルの単調増加秒タイムスタンプ。
// 検出不能時は 0 を返す。
func zeroCrossFreq(s, tsec []float64) float64 {
	if len(s) != len(tsec) || len(s) < 2 {
		return 0
	}
	var first, last float64
	n := 0
	for i := 1; i < len(s); i++ {
		if s[i-1] < 0 && s[i] >= 0 {
			// s[i-1] と s[i] の間で 0 を横切る点を線形補間
			denom := s[i] - s[i-1]
			frac := 0.0
			if denom != 0 {
				frac = -s[i-1] / denom
			}
			tc := tsec[i-1] + frac*(tsec[i]-tsec[i-1])
			if n == 0 {
				first = tc
			}
			last = tc
			n++
		}
	}
	if n < 2 {
		return 0
	}
	dt := last - first
	if dt <= 0 {
		return 0
	}
	return float64(n-1) / dt
}

// hann は長さ n の Hann 窓を返す。
func hann(n int) []float64 {
	w := make([]float64, n)
	if n == 1 {
		w[0] = 1
		return w
	}
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
	}
	return w
}

// spectrum は実信号の片側振幅スペクトル（長さ n/2+1）を返す。
// amp[k] は周波数 k*binHz の振幅推定（フルスケール正弦波 ≒ 1.0）。
// windowGain は窓のコヒーレントゲイン補正に使う窓係数の総和。
func spectrum(fft *fourier.FFT, windowed []float64, windowGain float64) (amp []float64, binHz float64, rate float64) {
	// rate は呼び出し側が設定する想定だが、ここでは coeff のみ計算する。
	coeff := fft.Coefficients(nil, windowed)
	amp = make([]float64, len(coeff))
	for k := range coeff {
		// 片側振幅: 2*|X|/sum(w)。DC/Nyquist は本来 1 倍だが PQ 用途では無視できる。
		amp[k] = 2 * cmplx.Abs(coeff[k]) / windowGain
	}
	return amp, 0, 0
}

// clusterAmp は中心 bin の周辺 ±span のパワーを合算した振幅を返す。
// Hann 窓のスペクトル漏れ（±1 bin への分散）をまとめて拾うため。
func clusterAmp(amp []float64, center, span int) float64 {
	lo := center - span
	hi := center + span
	if lo < 0 {
		lo = 0
	}
	if hi >= len(amp) {
		hi = len(amp) - 1
	}
	var p float64
	for k := lo; k <= hi; k++ {
		p += amp[k] * amp[k]
	}
	return math.Sqrt(p)
}

// nearestBin は周波数 f に最も近い bin 番号を返す。
func nearestBin(f, binHz float64) int {
	return int(math.Round(f / binHz))
}
