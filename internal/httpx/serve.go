package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ServerSpec describes one listener to run.
type ServerSpec struct {
	Name    string
	Addr    string
	Handler http.Handler
	// ReadTimeout bounds how long a client may take to send a request. It is
	// deliberately not applied to the whole response: a streaming completion
	// legitimately takes minutes.
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// Run starts every listener and blocks until the process receives SIGINT or
// SIGTERM, then drains connections within the grace period.
//
// Draining matters more here than in most services. A gateway shut down
// abruptly cuts every in-flight generation, and a caller cannot tell a
// truncated stream from a finished one without the terminating sentinel. The
// grace period must therefore be longer than the longest normal completion.
func Run(ctx context.Context, logger *slog.Logger, grace time.Duration, onShutdown func(), specs ...ServerSpec) error {
	if logger == nil {
		logger = slog.Default()
	}
	if grace <= 0 {
		grace = 30 * time.Second
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	servers := make([]*http.Server, 0, len(specs))
	errCh := make(chan error, len(specs))
	var wg sync.WaitGroup

	for _, spec := range specs {
		if spec.Addr == "" {
			continue
		}
		srv := &http.Server{
			Addr:              spec.Addr,
			Handler:           spec.Handler,
			ReadTimeout:       spec.ReadTimeout,
			ReadHeaderTimeout: orDuration(spec.ReadHeaderTimeout, 10*time.Second),
			WriteTimeout:      spec.WriteTimeout,
			IdleTimeout:       orDuration(spec.IdleTimeout, 120*time.Second),
		}
		servers = append(servers, srv)
		wg.Add(1)
		go func(name string, srv *http.Server) {
			defer wg.Done()
			logger.Info("listening", "server", name, "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(spec.Name, srv)
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining", "grace", grace)
	}

	// Readiness is failed first so the load balancer stops sending new work
	// while in-flight requests finish.
	if onShutdown != nil {
		onShutdown()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	var firstErr error
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	wg.Wait()
	logger.Info("shutdown complete")
	return firstErr
}

func orDuration(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
