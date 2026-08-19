package usage

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Server is the ingestion endpoint as a controller-runtime Runnable.
//
// It exists as a named type rather than a manager.RunnableFunc for one
// specific reason: controller-runtime treats any runnable that does not
// implement LeaderElectionRunnable as leader-only. Ingestion must run on
// *every* replica, because the Service load-balances agent reports across all
// of them -- if followers did not listen, half of every agent's reports would
// hit a closed port.
type Server struct {
	Addr  string
	Store *Store
}

// NeedLeaderElection reports false so followers serve ingestion too.
func (s *Server) NeedLeaderElection() bool { return false }

// Start runs the HTTP server until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           Handler(s.Store),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Collector expires old samples on a timer. Like the server it runs
// everywhere, since every replica holds its own buffer.
type Collector struct {
	Store    *Store
	Interval time.Duration
}

// NeedLeaderElection reports false: each replica must trim its own buffer.
func (c *Collector) NeedLeaderElection() bool { return false }

// Start runs the expiry loop until the context is cancelled.
func (c *Collector) Start(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.Store.GC()
		}
	}
}
