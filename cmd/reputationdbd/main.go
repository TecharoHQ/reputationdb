package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/svc"
	"github.com/facebookgo/flagenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	_ "github.com/joho/godotenv/autoload"
)

var (
	bind        = flag.String("bind", ":3823", "API HTTP host:port bind address")
	metricsBind = flag.String("metrics-bind", ":9090", "TCP address to serve the Prometheus /metrics endpoint; empty disables it")
	slogLevel   = flag.String("slog-level", "info", "Logging level")

	githubToken  = flag.String("github-token", "", "GitHub API token")
	tigrisBucket = flag.String("tigris-storage-bucket", "techaro-reputationdb", "Tigris bucket holding the published databases")
)

func main() {
	flagenv.Parse()
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	lg, err := internal.InitSlog(*slogLevel)
	if err != nil {
		log.Fatal(err)
	}

	if err := run(ctx, lg); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, lg *slog.Logger) error {
	cfg := &internal.Config{
		GitHubToken:  *githubToken,
		TigrisBucket: *tigrisBucket,
	}

	g, gCtx := errgroup.WithContext(ctx)

	if *bind != "" {
		ln, err := net.Listen("tcp", *bind)
		if err != nil {
			return fmt.Errorf("can't listen on %q: %w", *bind, err)
		}

		mux, err := svc.Route(ctx, lg, cfg)
		if err != nil {
			return err
		}
		p := new(http.Protocols)
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		srv := &http.Server{
			Handler:   mux,
			Protocols: p,
		}
		g.Go(func() error {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-gCtx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		})

		lg.Info("Listening", "bind", *bind)
	}

	if *metricsBind != "" {
		ln, err := net.Listen("tcp", *metricsBind)
		if err != nil {
			slog.Error("can't listen", "metrics_bind", *metricsBind, "err", err)
			os.Exit(1)
		}

		runtime.SetBlockProfileRate(100)
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

		srv := &http.Server{Handler: mux}
		g.Go(func() error {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-gCtx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		})
	}

	return g.Wait()
}
