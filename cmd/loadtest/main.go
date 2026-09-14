// Command loadtest measures the REST surface and the live channel under load.
//
//	go run ./cmd/loadtest --rest --athlete <id> --duration 30s --concurrency 50
//	go run ./cmd/loadtest --ws --session <id> --connections 500
//
// Written rather than pulled in, for two reasons. The WebSocket half has no
// off-the-shelf equivalent that does what is needed here -- open hundreds of
// connections, hold them, and report what each one actually received -- and
// the REST half is a hundred lines of standard library. A load tool that lives
// in the repository also runs in CI without installing anything.
//
// Percentiles are computed from every sample rather than from a summary,
// because the numbers that matter are the tail, and a tool that estimates its
// own tail is a tool that reports whatever its bucket boundaries decide.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr        = flag.String("addr", "localhost:8081", "API host:port")
		rest        = flag.Bool("rest", false, "run the REST load test")
		ws          = flag.Bool("ws", false, "run the WebSocket connection test")
		athlete     = flag.String("athlete", "", "athlete id for the REST mix")
		session     = flag.String("session", "", "session id for the live channel")
		duration    = flag.Duration("duration", 30*time.Second, "how long to run")
		concurrency = flag.Int("concurrency", 50, "concurrent REST workers")
		connections = flag.Int("connections", 500, "WebSocket connections to open")
		origin      = flag.String("origin", "http://localhost:5173", "Origin header")
	)
	flag.Parse()

	if !*rest && !*ws {
		flag.Usage()
		return errors.New("choose --rest or --ws")
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *rest {
		if *athlete == "" {
			return errors.New("--athlete is required for --rest")
		}
		if err := loadREST(ctx, *addr, *athlete, *session, *duration, *concurrency); err != nil {
			return err
		}
	}

	if *ws {
		if *session == "" {
			return errors.New("--session is required for --ws")
		}
		if err := loadWebSocket(ctx, *addr, *session, *origin, *connections, *duration); err != nil {
			return err
		}
	}

	return nil
}

// --- REST ------------------------------------------------------------------

type sample struct {
	route    string
	duration time.Duration
	status   int
}

func loadREST(
	ctx context.Context, addr, athlete, session string,
	duration time.Duration, concurrency int,
) error {
	// A mix rather than one endpoint. The expensive rollup and a cheap lookup
	// have very different shapes, and an average over only one of them
	// describes a system nobody is running.
	routes := []struct {
		name string
		path string
	}{
		{"analytics", "/api/v1/athletes/" + athlete + "/analytics?include=distribution,trend"},
		{"athlete", "/api/v1/athletes/" + athlete},
		{"sessions", "/api/v1/athletes/" + athlete + "/sessions?limit=20"},
		{"models", "/api/v1/models"},
	}
	if session != "" {
		routes = append(routes, struct{ name, path string }{
			"pitches", "/api/v1/sessions/" + session + "/pitches?limit=100&include=measurement,prediction",
		})
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        concurrency * 2,
			MaxIdleConnsPerHost: concurrency * 2,
		},
	}

	deadline, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	var (
		mu      sync.Mutex
		samples []sample
		wg      sync.WaitGroup
	)

	fmt.Printf("REST load: %d workers for %s against %s\n\n",
		concurrency, duration, addr)
	start := time.Now()

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			local := make([]sample, 0, 1024)
			i := worker

			for deadline.Err() == nil {
				route := routes[i%len(routes)]
				i++

				begin := time.Now()
				req, err := http.NewRequestWithContext(deadline, "GET",
					"http://"+addr+route.path, nil)
				if err != nil {
					continue
				}
				resp, err := client.Do(req)
				elapsed := time.Since(begin)

				status := 0
				if err == nil {
					status = resp.StatusCode
					_ = resp.Body.Close()
				}
				local = append(local, sample{route.name, elapsed, status})
			}

			mu.Lock()
			samples = append(samples, local...)
			mu.Unlock()
		}(w)
	}

	wg.Wait()
	elapsed := time.Since(start)

	report(samples, elapsed)
	return nil
}

func report(samples []sample, elapsed time.Duration) {
	if len(samples) == 0 {
		fmt.Println("no samples")
		return
	}

	byRoute := map[string][]sample{}
	var failures int
	for _, s := range samples {
		byRoute[s.route] = append(byRoute[s.route], s)
		// A cancelled request at the end of the window is not a failure; a
		// non-2xx is.
		if s.status != 0 && s.status >= 400 {
			failures++
		}
	}

	names := make([]string, 0, len(byRoute))
	for name := range byRoute {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Printf("%-12s %8s %10s %10s %10s %10s %10s\n",
		"route", "n", "p50", "p95", "p99", "max", "rps")
	for _, name := range names {
		rows := byRoute[name]
		ds := make([]time.Duration, len(rows))
		for i, r := range rows {
			ds[i] = r.duration
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })

		fmt.Printf("%-12s %8d %10s %10s %10s %10s %10.1f\n",
			name, len(ds),
			round(percentile(ds, 0.50)), round(percentile(ds, 0.95)),
			round(percentile(ds, 0.99)), round(ds[len(ds)-1]),
			float64(len(ds))/elapsed.Seconds())
	}

	all := make([]time.Duration, len(samples))
	for i, s := range samples {
		all[i] = s.duration
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	fmt.Printf("\n%-12s %8d %10s %10s %10s %10s %10.1f\n",
		"TOTAL", len(all),
		round(percentile(all, 0.50)), round(percentile(all, 0.95)),
		round(percentile(all, 0.99)), round(all[len(all)-1]),
		float64(len(all))/elapsed.Seconds())
	fmt.Printf("failures: %d (%.3f%%)\n", failures,
		100*float64(failures)/float64(len(samples)))
}

// percentile uses nearest-rank on the sorted samples, which needs no
// interpolation and cannot invent a value that was never measured.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func round(d time.Duration) string {
	if d < time.Millisecond {
		return d.Round(10 * time.Microsecond).String()
	}
	return d.Round(100 * time.Microsecond).String()
}

// --- WebSocket -------------------------------------------------------------

func loadWebSocket(
	ctx context.Context, addr, session, origin string,
	connections int, hold time.Duration,
) error {
	fmt.Printf("\nWebSocket load: opening %d connections to %s, holding %s\n\n",
		connections, addr, hold)

	var (
		opened    atomic.Int64
		refused   atomic.Int64
		snapshots atomic.Int64
		messages  atomic.Int64
		dropped   atomic.Int64
		wg        sync.WaitGroup
	)

	holdCtx, cancel := context.WithTimeout(ctx, hold)
	defer cancel()

	// The connections are closed by cancelling the hold, so every reader
	// leaves through its own error path rather than being interrupted.
	go func() {
		<-holdCtx.Done()
	}()

	url := "ws://" + addr + "/ws/sessions/" + session
	header := http.Header{"Origin": []string{origin}}

	dialer := *websocket.DefaultDialer
	// The default pool is small; without this the test measures the dialer.
	dialer.HandshakeTimeout = 20 * time.Second

	start := time.Now()

	for i := 0; i < connections; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, _, err := dialer.DialContext(holdCtx, url, header)
			if err != nil {
				refused.Add(1)
				return
			}
			opened.Add(1)
			defer func() { _ = conn.Close() }()

			// Closed when the hold expires, which is what ends the read loop.
			go func() {
				<-holdCtx.Done()
				_ = conn.Close()
			}()

			// One deadline for the whole hold, and no retry after it expires.
			// gorilla marks a connection failed on any read error, including a
			// timeout, and reading again panics -- which is how this tool
			// crashed the first time it opened five hundred sockets.
			_ = conn.SetReadDeadline(time.Now().Add(hold + 5*time.Second))

			for {
				_, raw, err := conn.ReadMessage()
				if err != nil {
					return
				}
				messages.Add(1)
				// Counted by shape rather than parsed: parsing every frame in
				// five hundred goroutines would make this a benchmark of the
				// JSON decoder.
				switch {
				case contains(raw, `"type":"session.snapshot"`):
					snapshots.Add(1)
				case contains(raw, `"code":"SLOW_CONSUMER"`):
					dropped.Add(1)
				}
			}
		}()

		// Opening five hundred sockets in a tight loop measures the accept
		// backlog rather than the server.
		if i%50 == 49 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	wg.Wait()
	elapsed := time.Since(start)

	fmt.Printf("  requested          %d\n", connections)
	fmt.Printf("  opened             %d\n", opened.Load())
	fmt.Printf("  refused            %d\n", refused.Load())
	fmt.Printf("  snapshots received %d\n", snapshots.Load())
	fmt.Printf("  messages received  %d\n", messages.Load())
	fmt.Printf("  slow-consumer warnings %d\n", dropped.Load())
	fmt.Printf("  goroutines in this process %d\n", runtime.NumGoroutine())
	fmt.Printf("  elapsed            %s\n", elapsed.Round(time.Millisecond))

	if opened.Load() < int64(connections) {
		fmt.Printf("\n  %d connection(s) were refused\n", refused.Load())
	}
	return nil
}

func contains(haystack []byte, needle string) bool {
	n := len(needle)
	for i := 0; i+n <= len(haystack); i++ {
		if string(haystack[i:i+n]) == needle {
			return true
		}
	}
	return false
}
