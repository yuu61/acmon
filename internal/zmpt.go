package acmon

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
)

// MCP3208 の SPI プロトコル定数とサンプリング既定値。
const (
	mcp3208StartSingle = 0x06 // 開始ビット + 単極性モード選択
	mcp3208ChMask      = 0x07 // 有効チャンネル範囲(0..7)マスク
	mcp3208ChHiMask    = 0x03 // 上位2bit配置用の下位マスク
	mcp3208ResultMask  = 0x0F // 12bit 変換結果の上位ニブルマスク
	mcp3208ChShift     = 6    // チャンネル下位2bitの配置シフト量
	mcp3208ByteShift   = 8    // 結果の上位/下位バイトを結合するシフト量
	mcp3208FrameBytes  = 3    // 1変換あたりの SPI 転送バイト数
	spiBitsPerWord     = 8    // SPI ワード長[bit]
	minWindowSamples   = 16   // 解析窓の最小サンプル数
	freq5060SplitHz    = 55.0 // これ未満なら 50Hz 系とみなす公称周波数の境界
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

// NewZmptSampler は ZMPT101B+MCP3208 サンプラを生成する。
func NewZmptSampler(cfg *Config, out *atomic.Pointer[ZmptSnapshot]) *ZmptSampler {
	return &ZmptSampler{cfg: cfg, out: out}
}

// readMCP3208 は単発変換コマンドを送り、12bit 値(0..4095)を返す。
// 単極性: tx[0]=0x06|(ch>>2), tx[1]=(ch&3)<<6, 結果=((rx[1]&0x0F)<<8)|rx[2].
func readMCP3208(conn spi.Conn, ch int, tx, rx []byte) (int, error) {
	c := byte(ch) & mcp3208ChMask //nolint:gosec // ch は 0..7 でマスク済み、byte 範囲に収まる
	tx[0] = mcp3208StartSingle | (c >> 2)
	tx[1] = (c & mcp3208ChHiMask) << mcp3208ChShift
	tx[2] = 0x00

	err := conn.Tx(tx, rx)
	if err != nil {
		return 0, fmt.Errorf("mcp3208 tx: %w", err)
	}

	return (int(rx[1]&mcp3208ResultMask) << mcp3208ByteShift) | int(rx[2]), nil
}

// Run は SPI を開いてサンプリングループを回す。ctx のキャンセルで終了する。
func (z *ZmptSampler) Run(ctx context.Context) error {
	conn, port, err := z.openSPI()
	if err != nil {
		return err
	}
	defer z.closePort(port)

	period := time.Second / time.Duration(z.cfg.ZmptRate)

	winLen := max(int(float64(z.cfg.ZmptRate)*z.cfg.ZmptWindow.Seconds()), minWindowSamples)

	raw := make([]float64, 0, winLen)
	tsec := make([]float64, 0, winLen)
	tx := make([]byte, mcp3208FrameBytes)
	rx := make([]byte, mcp3208FrameBytes)

	next := time.Now()
	winStart := time.Now()

	log.Printf("zmpt: started rate=%dSPS window=%v ch=%d", z.cfg.ZmptRate, z.cfg.ZmptWindow, z.cfg.ADCChannel)

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("zmpt run stopped: %w", err)
		}

		next = z.pace(next, period)
		ts := time.Now()

		v, err := readMCP3208(conn, z.cfg.ADCChannel, tx, rx)
		if err != nil {
			z.overruns++

			continue
		}

		raw = append(raw, float64(v))
		tsec = append(tsec, ts.Sub(winStart).Seconds())

		if len(raw) >= winLen {
			z.process(raw, tsec, ts, winStart)
			raw = raw[:0]
			tsec = tsec[:0]
			winStart = ts
		}
	}
}

// openSPI は SPI ポートを開いて接続を確立する。接続失敗時はポートを閉じてから返す。
//
//nolint:ireturn // periph API が spi.Conn / spi.PortCloser インターフェースを返すため不可避
func (z *ZmptSampler) openSPI() (spi.Conn, spi.PortCloser, error) {
	p, err := spireg.Open(z.cfg.SPIPort)
	if err != nil {
		return nil, nil, fmt.Errorf("spi open: %w", err)
	}

	conn, err := p.Connect(physic.Frequency(z.cfg.SPIHz)*physic.Hertz, spi.Mode0, spiBitsPerWord)
	if err != nil {
		z.closePort(p)

		return nil, nil, fmt.Errorf("spi connect: %w", err)
	}

	return conn, p, nil
}

// closePort は SPI ポートを閉じ、失敗時はログに残す。
func (*ZmptSampler) closePort(port spi.PortCloser) {
	if err := port.Close(); err != nil {
		log.Printf("zmpt: spi close: %v", err)
	}
}

// pace はデッドラインまで待機する簡易ペーサ。大きく遅延した場合は overrun を計上し、
// 基準時刻を現在時刻に再設定して返す（コアを焼かないため）。
func (z *ZmptSampler) pace(next time.Time, period time.Duration) time.Time {
	next = next.Add(period)

	d := time.Until(next)
	if d > 0 {
		time.Sleep(d)
	} else if -d > period {
		z.overruns++

		return time.Now()
	}

	return next
}

func (z *ZmptSampler) process(raw, tsec []float64, now, winStart time.Time) {
	s := removeDC(raw)
	rmsCounts := rmsValue(s)
	peakCounts := maxAbs(s)

	voltage := rmsCounts * z.cfg.ZmptCalVoltsPerCount

	crest := 0.0
	if rmsCounts > 0 {
		crest = peakCounts / rmsCounts
	}

	freq := zeroCrossFreq(s, tsec)
	dev := freqDeviation(freq)

	if z.cfg.ZmptCalVoltsPerCount > 0 {
		z.updateSagSwell(voltage)
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

// freqDeviation は検出周波数の公称値からの偏差を返す。西日本=60Hz を既定とし、
// 検出値が 50Hz 帯（東日本）なら自動で 50Hz 基準に切り替える。freq<=0 のときは 0。
func freqDeviation(freq float64) float64 {
	if freq <= 0 {
		return 0
	}

	nominalF := 60.0
	if freq < freq5060SplitHz {
		nominalF = 50.0
	}

	return freq - nominalF
}

// updateSagSwell は電圧しきい値とヒステリシスでサグ/スウェルのエッジを検出し計数する。
func (z *ZmptSampler) updateSagSwell(voltage float64) {
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
