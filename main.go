package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"periph.io/x/host/v3"
)

func main() {
	cfg := parseFlags()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("acmon: ")

	if cfg.EnableZMPT && cfg.ZmptCalVoltsPerCount <= 0 {
		log.Printf("WARNING: -zmpt-cal が未設定です。voltage_rms は 0 になり、サグ/スウェル判定も無効です（README の校正手順を参照）")
	}

	var zmptPtr atomic.Pointer[ZmptSnapshot]
	var scPtr atomic.Pointer[SoundcardSnapshot]

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(&zmptPtr, &scPtr))
	// 標準の process/go コレクタも付けておく
	reg.MustRegister(prometheus.NewGoCollector())

	if cfg.EnableZMPT {
		if _, err := host.Init(); err != nil {
			log.Fatalf("periph host init failed: %v", err)
		}
		z := NewZmptSampler(cfg, &zmptPtr)
		go func() {
			if err := z.Run(ctx); err != nil && ctx.Err() == nil {
				log.Fatalf("zmpt sampler stopped: %v", err)
			}
		}()
	}

	if cfg.EnableSoundcard {
		s := NewSoundcardSampler(cfg, &scPtr)
		go func() {
			if err := s.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("soundcard sampler stopped: %v", err)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("acmon power quality exporter\n/metrics\n"))
	})

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s/metrics", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutCtx, c2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer c2()
	srv.Shutdown(shutCtx)
	os.Exit(0)
}
