// Package main は acmon 電力品質エクスポータのエントリポイント。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"periph.io/x/host/v3"

	"acmon/internal"
)

const (
	httpReadTimeout     = 5 * time.Second
	httpWriteTimeout    = 10 * time.Second
	shutdownGracePeriod = 3 * time.Second
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("acmon: ")

	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

// run はエクスポータ本体を起動し、シグナルによる終了まで動かす。致命的な失敗のみ error を返す。
func run() error {
	cfg := acmon.ParseFlags()

	if cfg.EnableZMPT && cfg.ZmptCalVoltsPerCount <= 0 {
		log.Print("WARNING: -zmpt-cal が未設定です。voltage_rms は 0 になり、サグ/スウェル判定も無効です（README の校正手順を参照）")
	}

	var (
		zmptPtr atomic.Pointer[acmon.ZmptSnapshot]
		scPtr   atomic.Pointer[acmon.SoundcardSnapshot]
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	reg := prometheus.NewRegistry()
	reg.MustRegister(acmon.NewCollector(&zmptPtr, &scPtr))
	// 標準の process/go コレクタも付けておく。
	reg.MustRegister(collectors.NewGoCollector())

	// 致命的なバックグラウンド失敗(HTTP/ZMPT)を main へ伝える経路。
	fatal := make(chan error, 2)

	if cfg.EnableZMPT {
		if err := startZMPT(ctx, cfg, &zmptPtr, fatal); err != nil {
			return err
		}
	}

	if cfg.EnableSoundcard {
		startSoundcard(ctx, cfg, &scPtr)
	}

	srv := newServer(cfg.Listen, reg)
	go func() {
		fatal <- serve(srv)
	}()

	select {
	case <-ctx.Done():
		log.Print("shutting down")

		return shutdown(srv)
	case err := <-fatal:
		return err
	}
}

// startZMPT は periph を初期化し、ZMPT サンプラを別ゴルーチンで起動する。
// 想定外の停止は fatal 経路へ送り、プロセスを終了させる。
func startZMPT(
	ctx context.Context,
	cfg *acmon.Config,
	out *atomic.Pointer[acmon.ZmptSnapshot],
	fatal chan<- error,
) error {
	if _, err := host.Init(); err != nil {
		return fmt.Errorf("periph host init: %w", err)
	}

	z := acmon.NewZmptSampler(cfg, out)
	go func() {
		err := z.Run(ctx)
		if err != nil && ctx.Err() == nil {
			fatal <- fmt.Errorf("zmpt sampler stopped: %w", err)
		}
	}()

	return nil
}

// startSoundcard はサウンドカードサンプラを別ゴルーチンで起動する。失敗してもログのみで継続する。
func startSoundcard(ctx context.Context, cfg *acmon.Config, out *atomic.Pointer[acmon.SoundcardSnapshot]) {
	s := acmon.NewSoundcardSampler(cfg, out)
	go func() {
		err := s.Run(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("soundcard sampler stopped: %v", err)
		}
	}()
}

// newServer は /metrics を公開する HTTP サーバを構築する。
func newServer(addr string, reg *prometheus.Registry) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte("acmon power quality exporter\n/metrics\n"))
		if err != nil {
			log.Printf("write /: %v", err)
		}
	})

	return &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
	}
}

// serve は HTTP サーバを起動する。正常終了(ErrServerClosed)は nil、それ以外の失敗は error を返す。
func serve(srv *http.Server) error {
	log.Printf("listening on %s/metrics", srv.Addr)

	err := srv.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}

	return nil
}

// shutdown は猶予時間つきでサーバをグレースフルに停止する。
func shutdown(srv *http.Server) error {
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
	defer cancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}

	return nil
}
