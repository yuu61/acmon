package acmon

import (
	"testing"

	"periph.io/x/conn/v3"
	"periph.io/x/conn/v3/spi"
)

// fakeSPIConn は spi.Conn を満たすスタブ。最後の送信バイト列を記録し、
// 固定の受信バイト列を返す。MCP3208 のプロトコル（ビット組立・結果デコード）検証用。
type fakeSPIConn struct {
	lastW []byte
	resp  []byte
}

func (f *fakeSPIConn) Tx(w, r []byte) error {
	f.lastW = append([]byte(nil), w...)
	copy(r, f.resp)

	return nil
}

func (*fakeSPIConn) Duplex() conn.Duplex { return conn.Full }

func (*fakeSPIConn) String() string { return "fakeSPIConn" }

func (*fakeSPIConn) TxPackets(_ []spi.Packet) error { return nil }

// TestReadMCP3208 は単極性 3 バイト転送のコマンド組立とデコードを検証する。
// tx[0]=0x06|(ch>>2), tx[1]=(ch&3)<<6, 結果=((rx[1]&0x0F)<<8)|rx[2].
func TestReadMCP3208(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ch               int
		wantTx0, wantTx1 byte
	}{
		{0, 0x06, 0x00},
		{3, 0x06, 0xC0}, // ch3: 下位2bitを tx1 の上位へ
		{5, 0x07, 0x40}, // ch5: 最上位bitが tx0 側へ繰り上がる
		{7, 0x07, 0xC0},
	}

	for _, tc := range cases {
		// rx[1]=0xFA の上位ニブルはマスクで捨て、0x0ABC=2748 を得る。
		f := &fakeSPIConn{resp: []byte{0x00, 0xFA, 0xBC}}

		val, err := readMCP3208(f, tc.ch, make([]byte, 3), make([]byte, 3))
		if err != nil {
			t.Fatalf("ch %d: %v", tc.ch, err)
		}

		if f.lastW[0] != tc.wantTx0 || f.lastW[1] != tc.wantTx1 || f.lastW[2] != 0x00 {
			t.Errorf("ch %d: tx=% x, want [%x %x 00]", tc.ch, f.lastW, tc.wantTx0, tc.wantTx1)
		}

		if val != 0x0ABC {
			t.Errorf("ch %d: val=%d, want 2748", tc.ch, val)
		}
	}
}

func TestFreqDeviation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		freq, want float64
	}{
		{"60Hz nominal", 60, 0},
		{"50Hz nominal", 50, 0},
		{"slight high", 60.2, 0.2},
		{"slight low", 49.8, -0.2},
		{"no signal", 0, 0},
		{"54Hz uses 50 base", 54, 4},
		{"56Hz uses 60 base", 56, -4},
	}

	for _, tc := range cases {
		approx(t, tc.name, freqDeviation(tc.freq), tc.want, 1e-9)
	}
}

// TestUpdateSagSwell はしきい値とヒステリシスによるサグ/スウェルのエッジ計数を検証する。
func TestUpdateSagSwell(t *testing.T) {
	t.Parallel()

	z := &ZmptSampler{cfg: &Config{SagVolts: 95, SwellVolts: 107, EventHystV: 1.0}}

	z.updateSagSwell(100) // 正常

	if z.sagTotal != 0 || z.swellTotal != 0 {
		t.Fatalf("normal: sag=%d swell=%d, want 0/0", z.sagTotal, z.swellTotal)
	}

	z.updateSagSwell(90) // サグ突入（エッジで +1）
	z.updateSagSwell(90) // ラッチ中は加算しない

	if z.sagTotal != 1 {
		t.Errorf("sag latch: got %d, want 1", z.sagTotal)
	}

	z.updateSagSwell(95.5) // ヒステリシス帯（95<v<96）内はラッチ継続

	if !z.inSag {
		t.Error("should remain in sag at 95.5")
	}

	z.updateSagSwell(97) // しきい値+ヒステリシス超で復帰

	if z.inSag {
		t.Error("should clear sag above 96")
	}

	z.updateSagSwell(90) // 再突入で +1

	if z.sagTotal != 2 {
		t.Errorf("second sag: got %d, want 2", z.sagTotal)
	}

	z.updateSagSwell(110) // スウェル突入

	if z.swellTotal != 1 {
		t.Errorf("swell: got %d, want 1", z.swellTotal)
	}
}
