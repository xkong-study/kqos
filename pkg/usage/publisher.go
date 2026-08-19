package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/xkong-study/kqos/pkg/metrics"
)

// Publisher is the agent side of the data plane.
//
// It fans a report out to *every* controller replica rather than sending it
// through a load-balanced Service, and that is a deliberate correction to the
// obvious design.
//
// Only the elected leader runs the reconcilers that read the usage store, but
// a ClusterIP Service does not know which replica that is -- and because
// conntrack pins a client to whichever backend it first reached, an agent can
// spend its entire life reporting to a follower whose store nobody ever reads.
// The symptom is not an error anywhere: ingestion succeeds, metrics look
// healthy, and the workload profiles are simply always empty.
//
// Fanning out costs one small POST per replica and removes the failure mode
// entirely. It also means a replica that wins an election inherits a store
// that is already warm, instead of starting blind for a full retention window.
type Publisher struct {
	// endpoint is the base URL of the controller's ingestion address. When it
	// names a headless Service, every A record behind it is a replica.
	endpoint string
	client   *http.Client

	// fanout resolves the endpoint's host to every backing address.
	fanout bool

	mu         sync.Mutex
	cached     []string
	cachedAt   time.Time
	cacheFor   time.Duration
	resolveErr error
}

// NewPublisher builds a publisher aimed at the controller's ingestion address,
// e.g. "http://kqos-controller-usage.kqos-system.svc:8090". With fanout set,
// the host is resolved on every publish (memoised briefly) and the report is
// delivered to each address behind it.
func NewPublisher(endpoint string, timeout time.Duration, fanout bool) *Publisher {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Publisher{
		endpoint: endpoint,
		client:   &http.Client{Timeout: timeout},
		fanout:   fanout,
		cacheFor: 30 * time.Second,
	}
}

// Enabled reports whether an endpoint was configured. Usage publishing is
// optional: an agent with no controller to talk to still does its real job,
// which is keeping its own node healthy.
func (p *Publisher) Enabled() bool { return p != nil && p.endpoint != "" }

// Publish sends one report to every target.
//
// It returns an error only when *no* target accepted the report. A partial
// failure is not worth surfacing: the surviving replicas have the data, and
// the one that failed will be caught by its own liveness probe long before a
// gap in profiling matters.
func (p *Publisher) Publish(ctx context.Context, report Report) error {
	if !p.Enabled() {
		return nil
	}
	body, err := json.Marshal(report)
	if err != nil {
		metrics.UsageReportsTotal.WithLabelValues("send", "encode-error").Inc()
		return fmt.Errorf("encode usage report: %w", err)
	}

	targets, err := p.targets()
	if err != nil {
		metrics.UsageReportsTotal.WithLabelValues("send", "resolve-error").Inc()
		return fmt.Errorf("resolve usage endpoint: %w", err)
	}

	var lastErr error
	delivered := 0
	for _, target := range targets {
		if err := p.post(ctx, target, body); err != nil {
			lastErr = err
			continue
		}
		delivered++
	}

	if delivered == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no usage endpoints resolved from %s", p.endpoint)
		}
		return lastErr
	}
	return nil
}

func (p *Publisher) post(ctx context.Context, target string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+ReportPath, bytes.NewReader(body))
	if err != nil {
		metrics.UsageReportsTotal.WithLabelValues("send", "request-error").Inc()
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		metrics.UsageReportsTotal.WithLabelValues("send", "transport-error").Inc()
		return fmt.Errorf("publish to %s: %w", target, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 300 {
		metrics.UsageReportsTotal.WithLabelValues("send", "http-error").Inc()
		return fmt.Errorf("publish to %s: unexpected status %s", target, resp.Status)
	}
	metrics.UsageReportsTotal.WithLabelValues("send", "ok").Inc()
	return nil
}

// targets returns every base URL a report should be delivered to, resolving
// the configured host when fanout is enabled.
func (p *Publisher) targets() ([]string, error) {
	if !p.fanout {
		return []string{p.endpoint}, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if time.Since(p.cachedAt) < p.cacheFor && len(p.cached) > 0 {
		return p.cached, nil
	}

	u, err := url.Parse(p.endpoint)
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		// No explicit port: fall back to sending to the endpoint as written
		// rather than guessing a default.
		return []string{p.endpoint}, nil
	}

	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		// A DNS blip must not stop reporting. Keep using the last good answer
		// if there is one; otherwise fall back to the Service name and accept
		// that this report may reach only one replica.
		if len(p.cached) > 0 {
			return p.cached, nil
		}
		return []string{p.endpoint}, nil
	}

	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, u.Scheme+"://"+net.JoinHostPort(addr, port))
	}
	p.cached, p.cachedAt = out, time.Now()
	return out, nil
}

// Targets exposes the currently resolved endpoints, for logging and tests.
func (p *Publisher) Targets() []string {
	targets, err := p.targets()
	if err != nil {
		return nil
	}
	return targets
}
