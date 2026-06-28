package acmon

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
)

// ZmptSampler は MCP3208 を SPI で連続サンプリングし、窓ごとに
// RMS / 周波数 / クレストファクタ / サグ・スウェルを算出する。
type ZmptSampler struct {
	cfg *Config
	out *atomic.Pointer[ZmptSnapshot]

	// 累積カウンタ（このゴルーチンのみが書く）
	sagTotal   uint64
	swellTotal uint64
	overruns   uint64

	inSag   bool
	inSwell bool
}

func NewZmptSampler(cfg *Config, out *atomic.Pointer[ZmptSnapshot]) *ZmptSampler {
	return &ZmptSampler{cfg: cfg, out: out}
}

// readMCP3208 は単発変換コマンドを送り、12bit 値(0..4095)を返す。
// 単極性: tx[0]=0x06|(ch>>2), tx[1]=(ch&3)<<6, 結果=((rx[1]&0x0F)<<8)|rx[2]
func readMCP3208(conn spi.Conn, ch int, tx, rx []byte) (int, error) {
	c := byte(ch) & 0x07
	tx[0] = 0x06 | (c >> 2)
	tx[1] = (c & 0x03) << 6
	tx[2] = 0x00
	if err := conn.Tx(tx, rx); err != nil {
		return 0, err
	}
	return (int(rx[1]&0x0F) << 8) | int(rx[2]), nil
}

func (z *ZmptSampler) Run(ctx context.Context) error {
	p, err := spireg.Open(z.cfg.SPIPort)
	if err != nil {
		return err
	}
	defer p.Close()

	conn, err := p.Connect(physic.Frequency(z.cfg.SPIHz)*physic.Hertz, spi.Mode0, 8)
	if err != nil {
		return err
	}

	period := time.Second / time.Duration(z.cfg.ZmptRate)
	winLen := int(float64(z.cfg.ZmptRate) * z.cfg.ZmptWindow.Seconds())
	if winLen < 16 {
		winLen = 16
	}

	raw := make([]float64, 0, winLen)
	tsec := make([]float64, 0, winLen)
	tx := make([]byte, 3)
	rx := make([]byte, 3)

	next := time.Now()
	winStart := time.Now()
	maxCount := float64(int(1) << z.cfg.ADCBits) // 4096

	log.Printf("zmpt: started rate=%dSPS window=%v ch=%d", z.cfg.ZmptRate, z.cfg.ZmptWindow, z.cfg.ADCChannel)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// デッドラインペーシング（コアを焼かない簡易方式）
		next = next.Add(period)
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		} else if -d > period {
			// 大きく遅れた: オーバーラン計上して再同期
			z.overruns++
			next = time.Now()
		}

		ts := time.Now()
		v, err := readMCP3208(conn, z.cfg.ADCChannel, tx, rx)
		if err != nil {
			z.overruns++
			continue
		}
		raw = append(raw, float64(v))
		tsec = append(tsec, ts.Sub(winStart).Seconds())

		if len(raw) >= winLen {
			z.process(raw, tsec, ts, winStart, maxCount)
			raw = raw[:0]
			tsec = tsec[:0]
			winStart = ts
		}
	}
}

func (z *ZmptSampler) process(raw, tsec []float64, now, winStart time.Time, maxCount float64) {
	s, _ := removeDC(raw)
	rmsCounts := rmsValue(s)
	peakCounts := maxAbs(s)

	voltage := rmsCounts * z.cfg.ZmptCalVoltsPerCount
	crest := 0.0
	if rmsCounts > 0 {
		crest = peakCounts / rmsCounts
	}

	freq := zeroCrossFreq(s, tsec)
	// 周波数偏差は公称周波数に対して。西日本=60Hz 既定、
	// 検出値が 50Hz 帯（東日本）なら自動で 50Hz 基準に切り替える。
	dev := 0.0
	if freq > 0 {
		nominalF := 60.0
		if freq < 55.0 {
			nominalF = 50.0
		}
		dev = freq - nominalF
	}

	// サグ・スウェル（エッジトリガ + ヒステリシス）
	if z.cfg.ZmptCalVoltsPerCount > 0 {
		hyst := z.cfg.EventHystV
		if !z.inSag && voltage < z.cfg.SagVolts {
			z.inSag = true
			z.sagTotal++
		} else if z.inSag && voltage > z.cfg.SagVolts+hyst {
			z.inSag = false
		}
		if !z.inSwell && voltage > z.cfg.SwellVolts {
			z.inSwell = true
			z.swellTotal++
		} else if z.inSwell && voltage < z.cfg.SwellVolts-hyst {
			z.inSwell = false
		}
	}

	achieved := float64(len(raw)) / now.Sub(winStart).Seconds()

	snap := &ZmptSnapshot{
		VoltageRMS:    voltage,
		CrestFactor:   crest,
		FrequencyHz:   freq,
		FreqDeviation: dev,
		SagTotal:      z.sagTotal,
		SwellTotal:    z.swellTotal,
		SampleRate:    achieved,
		Overruns:      z.overruns,
		LastSample:    float64(now.UnixNano()) / 1e9,
		Valid:         true,
	}
	z.out.Store(snap)
}
