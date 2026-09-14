// Command wsclient watches one session's live channel from a terminal.
//
// It exists because the dashboard does not arrive until phase 9, and a channel
// that cannot be observed cannot be demonstrated or debugged. It is also the
// reference implementation of the client half of the contract: it reconnects
// with backoff, and it watches the sequence number for gaps rather than
// assuming every message arrives.
//
//	go run ./cmd/wsclient --session <uuid>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr = flag.String("addr", env("PITCHLAB_API_ADDR_PUBLIC", "localhost:8081"),
			"API host:port")
		session = flag.String("session", "", "session id to watch (required)")
		origin  = flag.String("origin", "http://localhost:5173",
			"Origin header; the server checks it")
		verbose = flag.Bool("verbose", false, "print the full payload of every message")
	)
	flag.Parse()

	if *session == "" {
		flag.Usage()
		return errors.New("--session is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	url := "ws://" + *addr + "/ws/sessions/" + *session

	// Reconnection is the client's responsibility, and it backs off: a server
	// that just restarted must not be met by every dashboard at once.
	backoff := time.Second
	for {
		err := watch(ctx, url, *origin, *verbose)
		if ctx.Err() != nil {
			return nil
		}
		fmt.Printf("disconnected: %v; retrying in %s\n", err, backoff.Round(time.Second))

		select {
		case <-time.After(backoff + time.Duration(rand.Int63n(int64(time.Second)))):
		case <-ctx.Done():
			return nil
		}

		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func watch(ctx context.Context, url, origin string, verbose bool) error {
	header := map[string][]string{"Origin": {origin}}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, url, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("%w (HTTP %d)", err, resp.StatusCode)
		}
		return err
	}
	defer func() { _ = conn.Close() }()

	fmt.Println("connected to", url)

	go func() {
		<-ctx.Done()
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
		_ = conn.Close()
	}()

	var expected uint64
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		var msg message
		if err := json.Unmarshal(raw, &msg); err != nil {
			fmt.Println("undecodable message:", err)
			continue
		}

		// A gap means the server dropped something for this connection. The
		// sequence number exists so the client can notice and reconcile over
		// REST rather than quietly showing an incomplete timeline.
		if msg.Seq > expected {
			fmt.Printf("  ! sequence gap: expected %d, got %d (%d messages missed)\n",
				expected, msg.Seq, msg.Seq-expected)
		}
		expected = msg.Seq + 1

		print(msg, raw, verbose)
	}
}

type message struct {
	Type          string          `json:"type"`
	Seq           uint64          `json:"seq"`
	TS            time.Time       `json:"ts"`
	CorrelationID string          `json:"correlation_id"`
	Data          json.RawMessage `json:"data"`
}

func print(msg message, raw []byte, verbose bool) {
	line := fmt.Sprintf("%4d  %-18s", msg.Seq, msg.Type)

	switch msg.Type {
	case "session.snapshot":
		var data struct {
			LastPitchIndex int             `json:"last_pitch_index"`
			RecentPitches  json.RawMessage `json:"recent_pitches"`
		}
		_ = json.Unmarshal(msg.Data, &data)
		line += fmt.Sprintf("last_pitch_index=%d", data.LastPitchIndex)

	case "pitch.analyzed":
		var data struct {
			Index       int32  `json:"session_pitch_index"`
			PitchType   string `json:"pitch_type"`
			Outcome     string `json:"actual_outcome"`
			Measurement struct {
				ReleaseSpeed *float64 `json:"release_speed"`
			} `json:"measurement"`
			Predictions []struct {
				Variant string  `json:"variant"`
				Score   float64 `json:"score"`
			} `json:"predictions"`
		}
		_ = json.Unmarshal(msg.Data, &data)

		speed := "   ? "
		if data.Measurement.ReleaseSpeed != nil {
			speed = fmt.Sprintf("%5.1f", *data.Measurement.ReleaseSpeed)
		}
		scores := make([]string, 0, len(data.Predictions))
		for _, p := range data.Predictions {
			scores = append(scores, fmt.Sprintf("%s=%.1f", p.Variant, p.Score))
		}
		line += fmt.Sprintf("#%-4d %-3s %s mph  %-16s %s",
			data.Index, data.PitchType, speed, data.Outcome,
			strings.Join(scores, " "))

	case "anomaly.detected":
		var data struct {
			Metric   string  `json:"metric"`
			ZRobust  float64 `json:"z_robust"`
			Severity string  `json:"severity"`
		}
		_ = json.Unmarshal(msg.Data, &data)
		line += fmt.Sprintf("%s z=%.2f %s", data.Metric, data.ZRobust, data.Severity)

	default:
		line += string(msg.Data)
	}

	fmt.Println(line)
	if verbose {
		fmt.Println("      ", string(raw))
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
