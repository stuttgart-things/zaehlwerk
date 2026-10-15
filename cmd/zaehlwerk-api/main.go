// Command zaehlwerk-api serves the ingest and match lifecycle endpoints.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	homerun "github.com/stuttgart-things/homerun-library/v4"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/live"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/panel"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
	"github.com/stuttgart-things/zaehlwerk/internal/ui"
)

const (
	defaultAddr      = ":8080"
	defaultRedisPort = "6379"
	shutdownTimeout  = 10 * time.Second
)

// Set by the linker at build time; see .ko.yaml. They are plain variables
// rather than constants because -X can only write to a variable, and they are
// deliberately left empty rather than given defaults here: api.BuildInfo
// decides what an unset field reads as, so there is one answer instead of two.
//
// A build that stamps nothing — `go build`, or ko outside the shared CI
// workflow — leaves all three empty, and /healthz then says dev rather than
// serving blank fields.
var (
	version string
	commit  string
	date    string
)

func main() {
	if err := run(); err != nil {
		slog.Error("zaehlwerk-api stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	sink := panelSink(log)
	if sink != nil {
		// Closed after the server, not with a defer here: a point still being
		// handled when SIGTERM arrives should reach the panel, and the drain
		// only works if nothing is left writing to the scorer.
		defer func() {
			if err := sink.Close(); err != nil {
				log.Warn("closing the panel sink", "error", err)
			}
		}()
	}

	switcher := panelSwitcher(log)
	if switcher != nil {
		// Closed after the server, so no request is still starting a match
		// while the panel is being given back.
		defer func() {
			if err := switcher.Close(); err != nil {
				log.Warn("closing the panel switcher", "error", err)
			}
		}()
	}

	// Off unless SCHMETTERPAUSE_URL is set, like every outbound coupling here
	// (invariant 4). Nil client, nil reporter, and the page asks for two
	// free-text names exactly as it does today.
	spClient := schmetterpauseClient(log)
	delay, err := reportDelay(log, spClient != nil)
	if err != nil {
		return err
	}

	hub := live.New()
	origins := allowedOrigins(log)
	beat, tuned := heartbeat(log)

	// The reporter is handed the match with each transition, so it needs no
	// registry and is built before it (#102).
	var reporter *schmetterpause.Reporter
	opts := registryOptions(sink, hub, switcher)
	if spClient != nil {
		reporter = schmetterpause.NewReporter(spClient,
			schmetterpause.WithLogger(log), schmetterpause.WithDelay(delay))
		// First, ahead of the hub: the page renders the result card from the
		// transition the hub hands it, and the card says when the result goes
		// out, so the reporter has to have decided that already.
		opts = append([]match.Option{match.WithMatchObserver(reporter.Observe)}, opts...)
	}
	registry := match.NewRegistry(opts...)

	apiOpts := []api.Option{
		api.WithLogger(log),
		api.WithHub(hub),
		api.WithAllowedOrigins(origins),
		api.WithPanelSwitcher(switcher),
	}
	uiOpts := []ui.Option{
		ui.WithLogger(log),
		ui.WithHub(hub),
	}
	if tuned {
		apiOpts = append(apiOpts, api.WithHeartbeat(beat))
		uiOpts = append(uiOpts, ui.WithHeartbeat(beat))
	}

	if opener := setOpener(log); opener != "" {
		apiOpts = append(apiOpts, api.WithSetOpener(opener))
	}

	apiOpts = append(apiOpts, api.WithBuildInfo(api.BuildInfo{
		Version: version, Commit: commit, Date: date,
	}))

	// Check-in needs Schmetterpause to say who a fob is, and a match from two
	// bare claims would have nobody to report to (ADR-0010). Each way in
	// needs its caller's token, or anybody on the network could check a
	// player in: the hub's for a fob, Schmetterpause's for a phone
	// (ADR-0011). Both claim the halves of one desk.
	if desk := checkinDesk(log, spClient); desk != nil {
		apiOpts = append(apiOpts,
			api.WithCheckin(desk, os.Getenv("CHECKIN_TOKEN")),
			api.WithPhoneCheckin(desk, os.Getenv("SCHMETTERPAUSE_TOKEN")))
		// The watch screen shows the halves when the table is free (#128).
		// The QR codes point at Schmetterpause's /table pages, so they are
		// only drawn when Schmetterpause can claim a half here, which is
		// the same condition the phone path has. SCHMETTERPAUSE_URL is the
		// address this service calls, and a phone has to reach it too: it
		// has to be the public one for the codes to work.
		phoneBase := ""
		if os.Getenv("SCHMETTERPAUSE_TOKEN") != "" {
			phoneBase = spClient.BaseURL()
		}
		uiOpts = append(uiOpts, ui.WithSides(desk, os.Getenv("CHECKIN_TOKEN") != "", phoneBase))
		if raw := os.Getenv("WATCH_RESULT_HOLD"); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				uiOpts = append(uiOpts, ui.WithResultHold(d))
			} else {
				log.Warn("ignoring WATCH_RESULT_HOLD, not a positive duration", "value", raw)
			}
		}
	}

	if spClient != nil {
		uiOpts = append(uiOpts,
			ui.WithSchmetterpause(spClient, reporter),
			ui.WithReportsTo(spClient.BaseURL()))
	}

	apiSrv := api.New(registry, apiOpts...)
	srv := &http.Server{
		Addr:    env("HTTP_ADDR", defaultAddr),
		Handler: routes(apiSrv, ui.New(registry, apiSrv, uiOpts...)),

		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Deliberately no WriteTimeout: the SSE endpoint holds a response open
		// for the length of a match, and a write deadline would cut it off.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Results still in their grace period go out now, alongside the server
	// draining rather than after it: a page holding its live stream keeps
	// Shutdown waiting for the whole timeout, and the results must not wait
	// behind it. A SIGKILL in the grace period loses them (ADR-0007).
	flushed := make(chan error, 1)
	go func() {
		if reporter == nil {
			flushed <- nil
			return
		}
		flushed <- reporter.Flush(shutdown)
	}()
	stopped := srv.Shutdown(shutdown)
	if err := <-flushed; err != nil {
		log.Error("results may not have reached schmetterpause", "error", err)
	}
	if stopped != nil {
		return fmt.Errorf("shutting down: %w", stopped)
	}
	return <-errs
}

// reportDelay reads SCHMETTERPAUSE_REPORT_DELAY, the grace period between a
// won match and its report (ADR-0007). Unset is the default and 0 reports at
// once. A value that is not a duration fails the start rather than being
// ignored like the other settings: guessing wrong here sends a result early,
// and a result sent cannot be taken back.
func reportDelay(log *slog.Logger, on bool) (time.Duration, error) {
	raw := os.Getenv("SCHMETTERPAUSE_REPORT_DELAY")
	if raw == "" {
		return schmetterpause.DefaultDelay, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("SCHMETTERPAUSE_REPORT_DELAY must be a duration of 0 or more, got %q", raw)
	}
	if on {
		log.Info("results are reported after a grace period", "report_delay", d)
	}
	return d, nil
}

// schmetterpauseClient builds the coupling, or reports that it is off.
//
// SCHMETTERPAUSE_URL turns it on, in the shape CATCHER_URL and
// OMNI_PITCHER_URL already have (ADR-0004). SCHMETTERPAUSE_TOKEN is the second
// half: that application does not register its /api routes without a token
// configured on its side, so without one here every call comes back 404 or
// 401. ADR-0004 named only the URL because the receiving surface did not exist
// yet; the token is that surface arriving, not a change of mind.
func schmetterpauseClient(log *slog.Logger) *schmetterpause.Client {
	url := os.Getenv("SCHMETTERPAUSE_URL")
	if url == "" {
		log.Info("schmetterpause handover disabled, SCHMETTERPAUSE_URL not configured")
		return nil
	}

	client, err := schmetterpause.New(schmetterpause.Config{
		BaseURL: url,
		Token:   os.Getenv("SCHMETTERPAUSE_TOKEN"),
	})
	if err != nil {
		log.Warn("schmetterpause handover disabled", "error", err)
		return nil
	}

	log.Info("schmetterpause handover enabled", "url", url,
		// Whether a token is set, never the token.
		"authenticated", os.Getenv("SCHMETTERPAUSE_TOKEN") != "")
	return client
}

// checkinDesk builds check-in per table half, or says it is off, in one line
// for each way in.
//
// The fob path needs CHECKIN_TOKEN, what the hub sends as a bearer: a
// check-in names a player to Schmetterpause, and an open route would take one
// from anybody on the network (#113). The phone path needs
// SCHMETTERPAUSE_TOKEN, what Schmetterpause sends when it claims a half for a
// phone check-in (ADR-0011); it does not need CHECKIN_TOKEN, which is the
// hub's. The desk exists while either is on.
//
// CHECKIN_TIMEOUT is how long a lone claim holds its half. CHECKIN_START is
// the phase a checked-in match starts in, serve_rally or warmup: nobody chose
// a first server, so it is always played for. CHECKIN_CHANGE_ENDS=true has
// the players change ends. A value that does not parse is ignored with a
// warning, like the other optional settings; none of them can send anything
// that cannot be taken back.
func checkinDesk(log *slog.Logger, sp *schmetterpause.Client) *checkin.Desk {
	fob := os.Getenv("CHECKIN_TOKEN") != ""
	phone := os.Getenv("SCHMETTERPAUSE_TOKEN") != ""
	switch {
	case sp == nil:
		log.Info("fob check-in disabled, a fob cannot be resolved without SCHMETTERPAUSE_URL")
		fob = false
	case !fob:
		log.Info("fob check-in disabled, CHECKIN_TOKEN not configured")
	}
	switch {
	case sp == nil:
		log.Info("phone check-in disabled, POST /checkin/claim needs SCHMETTERPAUSE_URL")
		phone = false
	case !phone:
		log.Info("phone check-in disabled, POST /checkin/claim needs SCHMETTERPAUSE_TOKEN")
	}
	if !fob && !phone {
		return nil
	}

	opts := []checkin.Option{checkin.WithLogger(log)}
	if raw := os.Getenv("CHECKIN_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			opts = append(opts, checkin.WithTimeout(d))
		} else {
			log.Warn("ignoring CHECKIN_TIMEOUT, not a positive duration", "value", raw)
		}
	}

	cfg := scorer.Config{Start: scorer.StartServeRally}
	switch raw := scorer.Start(os.Getenv("CHECKIN_START")); raw {
	case "":
	case scorer.StartServeRally, scorer.StartWarmup:
		cfg.Start = raw
	default:
		log.Warn("ignoring CHECKIN_START, must be serve_rally or warmup", "value", raw)
	}
	if raw := os.Getenv("CHECKIN_CHANGE_ENDS"); raw != "" {
		if on, err := strconv.ParseBool(raw); err == nil {
			cfg.ChangeEnds = on
		} else {
			log.Warn("ignoring CHECKIN_CHANGE_ENDS, not a boolean", "value", raw)
		}
	}
	opts = append(opts, checkin.WithMatch(cfg))

	desk := checkin.NewDesk(sp, opts...)
	settings := []any{"timeout", desk.Timeout(), "start", cfg.Start, "change_ends", cfg.ChangeEnds}
	if fob {
		log.Info("fob check-in enabled, POST /ingest/checkin with CHECKIN_TOKEN", settings...)
	}
	if phone {
		log.Info("phone check-in enabled, POST /checkin/claim with SCHMETTERPAUSE_TOKEN", settings...)
	}
	return desk
}

// setOpener reads SET_OPENER: who serves first in the sets after the first.
// Unset keeps the rulebook (ITTF law 2.13.6); "loser" is the house rule of a
// table where whoever lost the set opens the next one. Anything else is
// ignored with a warning rather than failing the start, like the other
// optional settings.
func setOpener(log *slog.Logger) scorer.SetOpener {
	switch raw := scorer.SetOpener(os.Getenv("SET_OPENER")); raw {
	case "":
		return ""
	case scorer.OpenerAlternate, scorer.OpenerLoser:
		log.Info("set opener configured", "set_opener", raw)
		return raw
	default:
		log.Warn("ignoring SET_OPENER, must be alternate or loser", "value", raw)
		return ""
	}
}

func registryOptions(sink *panel.Sink, hub *live.Hub, switcher *panel.Switcher) []match.Option {
	var opts []match.Option

	if raw := os.Getenv("MAX_RETAINED_MATCHES"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			opts = append(opts, match.WithMaxRetained(n))
		} else {
			slog.Warn("ignoring MAX_RETAINED_MATCHES, not a positive number", "value", raw)
		}
	}
	if sink != nil {
		opts = append(opts, match.WithMatchObserver(sink.Observe))
	}
	if hub != nil {
		opts = append(opts, match.WithObserver(hub.Observe))
	}
	if switcher != nil {
		// For the inactivity clock, and to give the panel back when a match is
		// won without anyone calling /end.
		opts = append(opts, match.WithObserver(switcher.Observe))
	}
	return opts
}

// panelSwitcher builds the LED catcher stream switcher, or returns nil when no
// catcher is configured — a local run should not need one.
func panelSwitcher(log *slog.Logger) *panel.Switcher {
	addr := os.Getenv("CATCHER_URL")
	if addr == "" {
		log.Info("panel stream switching disabled, no CATCHER_URL configured")
		return nil
	}

	cfg := panel.StreamConfig{
		BaseURL:      strings.TrimRight(addr, "/"),
		MatchStreams: splitList(os.Getenv("CATCHER_MATCH_STREAMS")),
		IdleStreams:  splitList(os.Getenv("CATCHER_IDLE_STREAMS")),
		Logger:       log,
	}
	if raw := os.Getenv("CATCHER_IDLE_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			log.Warn("ignoring CATCHER_IDLE_TIMEOUT, not a positive duration", "value", raw)
		} else {
			cfg.IdleTimeout = d
		}
	}

	s := panel.NewSwitcher(cfg)
	log.Info("panel stream switching enabled", "catcher", cfg.BaseURL)
	return s
}

func splitList(raw string) []string {
	var out []string
	for _, v := range strings.Split(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// heartbeat reads the live stream tuning that depends on what sits in front of
// the service, reporting whether it was configured at all. The default suits a
// proxy that gives an idle connection a minute; one that is stricter needs a
// shorter interval, and there is no way to find that out from in here.
//
// It applies to both streams. They are the same connection through the same
// proxy, and a browser watching the score over one of them is no more patient
// than a browser watching it over the other.
func heartbeat(log *slog.Logger) (time.Duration, bool) {
	raw := os.Getenv("STREAM_HEARTBEAT")
	if raw == "" {
		return 0, false
	}

	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Warn("ignoring STREAM_HEARTBEAT, not a positive duration", "value", raw)
		return 0, false
	}
	log.Info("live stream heartbeat", "interval", d)
	return d, true
}

// routes puts the browser UI next to the JSON API in one handler.
//
// The two are separate servers because they answer in different languages —
// JSON for the hub, the firmware and Schmetterpause, HTML partials for the
// page — and they meet here rather than one mounting the other: the UI drives
// the API's match lifecycle, so anything else would be a circle.
//
// Everything not under /ui is the API's, including the unknown paths it
// answers 404 for. The root is the one exception, because someone who types
// the host and port is looking for the page.
func routes(apiSrv, uiSrv http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", apiSrv)
	mux.Handle("/ui", uiSrv)
	mux.Handle("/ui/", uiSrv)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})
	return mux
}

// allowedOrigins reads the browser origins that may read the live stream.
//
// Empty means no cross-origin browser may read it, which is the right default
// for a service that is reachable on the office network: a same-origin page
// and anything server-side still work, and Schmetterpause is named explicitly
// when it is deployed.
func allowedOrigins(log *slog.Logger) []string {
	raw := os.Getenv("ALLOWED_ORIGINS")
	if raw == "" {
		log.Info("no ALLOWED_ORIGINS configured, cross-origin browsers cannot read the live stream")
		return nil
	}

	var origins []string
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	log.Info("live stream origins allowed", "origins", origins)
	return origins
}

// panelSink builds the LED panel sink, or returns nil when neither backend is
// configured. A local run should need neither: without them the score simply
// never leaves the API, which is the right default for developing against the
// endpoints alone.
//
// There are two ways onto the bus. OMNI_PITCHER_URL posts to
// homerun2-omni-pitcher over HTTP, which is what a zaehlwerk by the table
// needs — Redis in the cluster is a ClusterIP service and not reachable from
// there, while omni-pitcher has a route and a token. REDIS_ADDR writes to
// Redis directly, which is fewer moving parts when there is a Redis to reach.
//
// Both configured is a mistake worth naming rather than resolving quietly, so
// it says which one it took.
func panelSink(log *slog.Logger) *panel.Sink {
	stream := env("PANEL_STREAM", panel.DefaultStream)

	if url := os.Getenv("OMNI_PITCHER_URL"); url != "" {
		if os.Getenv("REDIS_ADDR") != "" {
			log.Warn("both OMNI_PITCHER_URL and REDIS_ADDR are set, using OMNI_PITCHER_URL")
		}

		pitcher := panel.NewHTTPPitcher(panel.HTTPPitcherConfig{
			BaseURL: url,
			Path:    env("OMNI_PITCHER_PATH", panel.DefaultPitchPath),
			Token:   os.Getenv("OMNI_PITCHER_TOKEN"),
			// The server decides the stream; this is only so a landing on the
			// wrong one is noticed rather than silently invisible.
			ExpectStream: stream,
			Logger:       log,
		})
		log.Info("panel enabled via omni-pitcher", "url", url, "expected_stream", stream,
			"authenticated", os.Getenv("OMNI_PITCHER_TOKEN") != "")

		return panel.New(pitcher, panel.Config{Stream: stream, Logger: log})
	}

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		log.Info("panel disabled, neither OMNI_PITCHER_URL nor REDIS_ADDR configured")
		return nil
	}

	rc := homerun.RedisConfig{
		Addr:     addr,
		Port:     env("REDIS_PORT", defaultRedisPort),
		Password: os.Getenv("REDIS_PASSWORD"),
		Stream:   stream,
	}
	log.Info("panel enabled via redis", "redis", rc.Addr+":"+rc.Port, "stream", rc.Stream)

	// One pitcher for the process lifetime. NewPitcher opens a connection pool,
	// so the per-message form would open and leak one per point.
	return panel.New(homerun.NewPitcher(rc), panel.Config{
		Stream: rc.Stream,
		Logger: log,
	})
}

func logLevel() slog.Level {
	var level slog.Level
	if raw := os.Getenv("LOG_LEVEL"); raw != "" {
		if err := level.UnmarshalText([]byte(raw)); err != nil {
			slog.Warn("ignoring LOG_LEVEL, not a level", "value", raw)
		}
	}
	return level
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
