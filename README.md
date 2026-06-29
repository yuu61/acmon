# acmon — AC100V 電源品質モニタ exporter

Raspberry Pi で商用電源（AC100V）の **電圧品質だけ** を観測し、Prometheus 形式の
`/metrics` を1本生やす常駐 exporter。下流に負荷を繋がない「電圧プローブ」用途。
Prometheus は直接 scrape、Zabbix は HTTP agent + Prometheus preprocessing で同じ
エンドポイントを食える。

2系統のフロントエンドを独立に走らせ、各値に `source` ラベルを付けて同居させる。

| source | フロントエンド | 主担当メトリクス |
|---|---|---|
| `zmpt` | ZMPT101B → MCP3208 (12bit SAR) + 精密VREF | `voltage_rms` / `frequency` / `crest_factor` / `sag`・`swell` |
| `soundcard` | 降圧トランス二次 → USBサウンドカード (ALSA) | `thd` / `harmonic_ratio{order}` / `noise_floor` / `flicker_pst` / `transient` |

周波数は両系統で出せるが二重計上を避けるため **ZMPT を正** とし、`frequency_*` は
`source="zmpt"` のみ出力する。

---

## ⚠️ 安全（最優先）

- 一次側（AC100V）と Pi の GND を **絶対に直結しない**。両系統とも絶縁トランスを介す。
- ZMPT101B は内部にトランスを内蔵。サウンドカード系統は **別個の降圧トランス**で取り、
  各系統の絶縁を独立させる（トランス2個構成が最も素直で安全マージンも高い）。
- 西日本（神戸）は **60Hz**。本exporterは検出周波数が 55Hz 未満なら自動で 50Hz 基準に切替。
- 通電状態での配線・調整は行わない。

---

## 配線

### ZMPT101B → MCP3208

ZMPT モジュールのゲインポットで「100V RMS → ADC入力で約 1.0V RMS、1.5V バイアス中心」
に合わせる（ピークは 1.5±1.41V で 0〜3V に収まる）。

MCP3208（DIP-16）の結線:

| MCP3208 | 接続先 |
|---|---|
| VDD (16) | 3.3V |
| VREF (15) | **精密リファレンス**（REF3030 = 3.0V 推奨。Pi の 3.3V は使わない） |
| AGND (14) | GND |
| CLK (13) | SPI0 SCLK (GPIO11 / pin23) |
| Dout (12) | SPI0 MISO (GPIO9 / pin21) |
| Din (11) | SPI0 MOSI (GPIO10 / pin19) |
| CS (10) | SPI0 CE0 (GPIO8 / pin24) |
| DGND (9) | GND |
| CH0 (1) | ZMPT 出力（バイアス済み） |

> **精度を律速するのは VREF の安定性**であってビット数ではない。Pi の 3.3V レールは
> スイッチングで数十mV泳ぐので、ここに REF3030 / MCP1501 / ADR4540 等を奢ること。
> これで校正後 0.1% 級が現実的。

`raspi-config` か `/boot/firmware/config.txt` の `dtparam=spi=on` で SPI を有効化し、
`/dev/spidev0.0` を確認。

### 降圧トランス二次 → USBサウンドカード

二次側を分圧 + AC結合してサウンドカードの **ライン入力**（マイク入力は不可、AGCと
過大ゲインで歪む）へ。入力がフルスケールの 50〜70% に収まるよう分圧比を調整。
`arecord -l` でデバイス名（例 `hw:1,0`）を確認。

---

## ビルド

ピュア Go（cgo 不要）。任意のマシンから単一バイナリにクロスコンパイルできる。

```sh
go mod tidy

# Pi 4 / Pi 5（64bit OS）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o acmon ./cmd/acmon

# Pi（32bit OS）/ Zero 2W（32bit）
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o acmon ./cmd/acmon
```

ランタイム依存: サウンドカード系統を使う場合 Pi 側に **alsa-utils**（`arecord`）が必要。

```sh
sudo apt-get install -y alsa-utils
```

---

## 校正（voltage_rms を使うなら必須）

`voltage_rms = RMSカウント × zmpt-cal`。係数 `-zmpt-cal` を実測で求める。

1. 係数 1.0 で起動: `./acmon -zmpt-cal 1.0 -enable-soundcard=false`
2. `curl -s localhost:9100/metrics | grep voltage_rms` で表示値 R（= RMSカウント）を読む。
3. 同時に較正済みマルチメータで実際の系統電圧 V を測る。
4. `zmpt-cal = V / R` を本番フラグに設定。

未校正（`-zmpt-cal` 未設定）だと `voltage_rms` は 0 になり、サグ/スウェル判定も無効化される
（誤検出防止）。起動時に WARNING を出す。

---

## 起動例

```sh
./acmon \
  -listen :9100 \
  -spi-port /dev/spidev0.0 -adc-channel 0 \
  -zmpt-rate 8000 -zmpt-cal 0.0732 \
  -sag-volts 95 -swell-volts 107 \
  -audio-device hw:1,0 -audio-rate 48000 -audio-format S32_LE -fft-size 8192
```

主なフラグ（`-h` で全件）:

| フラグ | 既定 | 説明 |
|---|---|---|
| `-listen` | `:9100` | `/metrics` の待受 |
| `-enable-zmpt` / `-enable-soundcard` | true | 各系統の有効化（片系統テストに） |
| `-zmpt-rate` | 8000 | ZMPT 目標サンプルレート[SPS] |
| `-zmpt-cal` | 0 | 校正係数（上記手順で決定） |
| `-sag-volts` / `-swell-volts` | 95 / 107 | サグ/スウェル閾値[V] |
| `-audio-device` | default | `arecord -D` のデバイス |
| `-fft-size` | 8192 | FFTサイズ（2のべき乗） |
| `-transient-k` | 1.6 | 期待ピークの何倍を過渡とみなすか |
| `-pst-gain` | 50 | 簡易フリッカ指標のスケール（現場調整） |

CPU負荷は連続FFTが支配的。Pi 4/5 なら両系統同時で余裕。Zero 2W で苦しければ
`acmon_buffer_overruns_total` を見ながら `-fft-size` か `-zmpt-rate` を下げる。

---

## systemd

`/etc/systemd/system/acmon.service`:

```ini
[Unit]
Description=acmon power quality exporter
After=network.target sound.target

[Service]
ExecStart=/usr/local/bin/acmon -zmpt-cal 0.0732 -audio-device hw:1,0
Restart=always
RestartSec=2
# SPI/audio アクセスのためグループを付与
SupplementaryGroups=spi audio
User=acmon

[Install]
WantedBy=multi-user.target
```

```sh
sudo useradd -r -s /usr/sbin/nologin acmon
sudo systemctl enable --now acmon
```

---

## Prometheus

```yaml
scrape_configs:
  - job_name: acmon
    scrape_interval: 15s
    static_configs:
      - targets: ['pi-acmon.lan:9100']
```

staleness 検出（サンプリング停止＝古い値を返し続ける事故の検知）:

```promql
time() - acmon_last_sample_timestamp_seconds > 60
```

---

## Zabbix（同じ /metrics を使う）

Zabbix は Prometheus preprocessing 内蔵。**master item で1回 fetch → dependent item に分配**。

### 1. master item

- タイプ: HTTP agent
- URL: `http://pi-acmon.lan:9100/metrics`
- 更新間隔: 15s
- 履歴保存: 0（生テキストは保持不要）

### 2. dependent item（各値）

master を親に、preprocessing 1ステップ **Prometheus pattern**:

| item | パターン |
|---|---|
| 電圧RMS | `acmon_voltage_rms_volts{source="zmpt"}` |
| 周波数 | `acmon_frequency_hertz` |
| THD | `acmon_thd_ratio` |
| 5次高調波 | `acmon_harmonic_ratio{order="5"}` |
| サグ累計 | `acmon_sag_events_total` |

サグ/スウェル/過渡は counter なので、トリガで `change()` / `last()-prev` を見て
「scrape 間で発生したか」を判定する。

### 3. 高調波の LLD（次数を自動展開）

- discovery rule: master 依存、preprocessing **Prometheus to JSON**、パターン `acmon_harmonic_ratio`
- LLD マクロ: `{#ORDER}` ← JSONPath `$.labels.order`
- item プロトタイプ: preprocessing **Prometheus pattern** `acmon_harmonic_ratio{order="{#ORDER}"}`

これで scrape あたり HTTP は1回、Zabbix 側は任意個に展開できる。

---

## メトリクス一覧

```
# gauge
acmon_voltage_rms_volts{source="zmpt"}
acmon_crest_factor{source="zmpt"}
acmon_frequency_hertz{source="zmpt"}
acmon_frequency_deviation_hertz{source="zmpt"}
acmon_thd_ratio{source="soundcard"}
acmon_harmonic_ratio{source="soundcard",order="3|5|7|9|11|13|15"}
acmon_noise_floor_dbfs{source="soundcard"}
acmon_flicker_pst{source="soundcard"}        # 簡易指標（IEC 61000-4-15 正式版ではない）
acmon_sample_rate_hertz{source="..."}

# counter
acmon_sag_events_total{source="zmpt"}
acmon_swell_events_total{source="zmpt"}
acmon_transient_events_total{source="soundcard"}
acmon_buffer_overruns_total{source="..."}

# health
acmon_last_sample_timestamp_seconds{source="..."}
acmon_cpu_temp_celsius
```

---

## 設計上の注記

- `min`/`max`・最悪値は持たない。トレンド集約は Zabbix/Prometheus 側（`min_over_time` 等）に委ねる。
  scrape 間の瞬間異常は **イベントカウンタ**側で保持する設計。
- `flicker_pst` は基本波振幅の相対変動から作る**近似指標**で、IEC 61000-4-15 の重み付け
  フィルタ・分類器は実装していない。傾向監視用。`-pst-gain` で現場スケール調整。
- ノイズ床は AC結合・窓・FFT 正規化に依存する相対値（フルスケール正弦波 ≒ 0 dBFS）。
- 生波形・全FFTビンは `/metrics` に載せない（TSDB 肥大化回避）。必要なら別経路で。
```
