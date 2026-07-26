package svc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/validate"
	"connectrpc.com/vanguard"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal"
	freefetchv1 "github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/svc/freefetch/v1"
	"github.com/TecharoHQ/reputationdb/gen"
	freefetchv1connect "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/free/fetch/v1/fetchv1connect"
	"github.com/mdigger/rpclog"
)

func Route(ctx context.Context, lg *slog.Logger, cfg *internal.Config) (http.Handler, error) {
	mux := http.NewServeMux()
	logger := rpclog.New(lg)

	errs := []error{}
	var svcs []*vanguard.Service

	{
		freeFetchSvc, err := freefetchv1.New(ctx, lg, cfg)
		if err != nil {
			errs = append(errs, fmt.Errorf("can't construct freefetchv1 service: %w", err))
		}

		path, handler := freefetchv1connect.NewFetchServiceHandler(
			freeFetchSvc,
			connect.WithInterceptors(logger, validate.NewInterceptor()),
		)
		mux.Handle(path, handler)

		svc := vanguard.NewService(path, handler)
		svcs = append(svcs, svc)
	}

	transcoder, err := vanguard.NewTranscoder(svcs)
	if err != nil {
		errs = append(errs, fmt.Errorf("can't create vanguard transcoder: %w", err))
	}

	mux.HandleFunc("/api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, gen.Static, "openapi.yaml")
	})

	mux.Handle("/", transcoder)

	if len(errs) != 0 {
		return nil, fmt.Errorf("can't build API server: %w", errors.Join(errs...))
	}

	return mux, nil
}
