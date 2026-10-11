// Command merge runs the SmartClass segment merge service.
//
// Subcommands: run, status, doctor, migrate, register-permissions. Run `merge`
// without arguments to print the usage; unknown subcommands exit with status 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/crazy4chicken/smartclass-merge/internal/config"
	"github.com/crazy4chicken/smartclass-merge/internal/ffmpeg"
	"github.com/crazy4chicken/smartclass-merge/internal/filehouse"
	"github.com/crazy4chicken/smartclass-merge/internal/httpapi"
	"github.com/crazy4chicken/smartclass-merge/internal/iamauth"
	"github.com/crazy4chicken/smartclass-merge/internal/merge"
	"github.com/crazy4chicken/smartclass-merge/internal/scheduler"
	"github.com/crazy4chicken/smartclass-merge/internal/store"
)

// version is set at build time with -ldflags "-X main.version=<version>".
var version = "dev"

const (
	serviceName     = "merge"
	shutdownTimeout = 10 * time.Second
	doctorTimeout   = 30 * time.Second
	registerTimeout = 60 * time.Second
	drainTimeout    = 2 * time.Second
	// probeTimeout bounds one dependency probe in status and doctor.
	probeTimeout = 5 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches one subcommand and returns the process exit code.
func run(args []string) int {
	if len(args) > 0 && (args[0] == "-version" || args[0] == "--version") {
		fmt.Println(version)
		return 0
	}
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	command := args[0]
	args = args[1:]
	switch command {
	case "-h", "-help", "--help":
		usage(os.Stdout)
		return 0
	case "run", "status", "doctor", "register-permissions", "migrate":
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", command)
		usage(os.Stderr)
		return 2
	}

	flags := flag.NewFlagSet(serviceName+" "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	adminToken := ""
	if command == "register-permissions" {
		flags.StringVar(&adminToken, "token", os.Getenv(config.EnvTeamusersAdminToken), "teamusers admin bearer token")
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	if err := validateCommandConfig(command, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	logger := newLogger(cfg)

	switch command {
	case "run":
		return runServer(cfg, logger)
	case "status":
		return runStatus(cfg, logger)
	case "doctor":
		return runDoctor(cfg, logger)
	case "register-permissions":
		return runRegisterPermissions(cfg, logger, adminToken)
	case "migrate":
		return runMigrate(cfg, logger)
	}
	// Unreachable: every command was validated before the configuration was
	// loaded, so the switch above always returns. This keeps the compiler happy
	// if a command is ever added to only one of the two switches.
	return 2
}

// validateCommandConfig enforces the runtime dependencies of one subcommand.
// Load has already rejected malformed values, so a command only refuses to start
// over what it truly cannot run without: the server needs the database and the
// destination bucket (plus the endpoints and credentials unless dev mode is on),
// migrate needs the database and register-permissions needs the teamusers base
// URL. status and doctor need none of them, because they report a missing
// dependency as SKIP instead of failing.
func validateCommandConfig(command string, cfg config.Config) error {
	switch command {
	case "run":
		return cfg.ValidateServer()
	case "migrate":
		return cfg.ValidateDatabase()
	case "register-permissions":
		return cfg.ValidateTeamusers()
	default:
		return nil
	}
}

// usage prints the subcommand summary.
func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: %s <command>

commands:
  run                   serve the HTTP API and run the merge queue
  status                print the redacted configuration and dependency reachability
  doctor                run the status checks with a %s timeout; exit non-zero on failure
  migrate               apply the embedded goose migrations and exit
  register-permissions  upsert the merge:* permission catalog in teamusers (-token)

`, serviceName, doctorTimeout)
}

// newLogger builds the JSON logger at the configured level.
func newLogger(cfg config.Config) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.Level()}))
}

// iamauthOptions maps the service configuration onto the IAM wiring.
func iamauthOptions(cfg config.Config, log *slog.Logger) iamauth.Options {
	return iamauth.Options{
		BaseURL:      cfg.TeamusersURL,
		Issuer:       cfg.TeamusersIssuer,
		Audience:     cfg.TeamusersAudience,
		ClientID:     cfg.TeamusersClientID,
		ClientSecret: cfg.TeamusersClientSecret,
		Timeout:      cfg.TeamusersTimeout,
		Logger:       log,
		Dev:          cfg.Dev,
	}
}

// runServer serves traffic and runs the merge queue until a signal arrives.
func runServer(cfg config.Config, log *slog.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.New(ctx, cfg.DSN)
	if err != nil {
		log.Error("open database", "error", err)
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Error("apply migrations", "error", err)
		return 1
	}

	auth, err := iamauth.New(iamauthOptions(cfg, log))
	if err != nil {
		log.Error("init iam authorizer", "error", err)
		return 1
	}
	defer auth.Close()

	objects := filehouse.New(cfg.FilehouseURL, auth, cfg.FilehouseTimeout, log)
	binary, err := ffmpeg.New(ffmpeg.Options{
		BinaryPath: cfg.FFmpegPath,
		WorkDir:    cfg.WorkDir,
		Logger:     log,
	})
	if err != nil {
		log.Error("init ffmpeg", "error", err)
		return 1
	}
	engine := merge.New(merge.Deps{Filehouse: objects, FFmpeg: binary, Logger: log}, merge.Options{
		FPS:               cfg.DefaultFPS,
		TranscodeFallback: cfg.TranscodeFallback,
		WorkDir:           cfg.WorkDir,
		ObjectMaxBytes:    cfg.ObjectMaxBytes,
		PartBytes:         cfg.MultipartPartBytes,
	})
	queue := scheduler.New(scheduler.Options{
		Tick:          cfg.SchedTick,
		Lease:         cfg.JobLease,
		JobTimeout:    cfg.JobTimeout,
		MaxAttempts:   cfg.MaxAttempts,
		MaxConcurrent: cfg.MaxConcurrentJobs,
		RetryBackoff:  cfg.RetryBackoff,
	}, db, engine, log)

	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		queue.Run(ctx)
	}()

	handler := httpapi.New(httpapi.Deps{
		Store:               db,
		Authorizer:          auth,
		Scheduler:           queue,
		Filehouse:           objects,
		FFmpeg:              binary,
		DefaultSourceBucket: cfg.SourceBucket,
		DefaultDestBucket:   cfg.DestBucket,
		DefaultFPS:          cfg.DefaultFPS,
		MaxAttempts:         cfg.MaxAttempts,
		WorkDir:             cfg.WorkDir,
		Version:             version,
		Logger:              log,
	})
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("listen", "addr", cfg.ListenAddr, "error", err)
		return 1
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	log.Info("merge serving", "addr", listener.Addr().String(), "version", version, "dev", cfg.Dev)

	failure := false
	schedulerDrained := false
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "error", err)
			failure = true
		}
	case <-schedulerDone:
		schedulerDrained = true
		if ctx.Err() == nil {
			// Run blocks until its context is cancelled, so a scheduler that
			// returns on its own means the queue stopped claiming jobs and
			// already-requested merges would wait forever.
			log.Error("scheduler stopped unexpectedly")
			failure = true
		}
	case <-ctx.Done():
	}
	stop()
	if !schedulerDrained {
		select {
		case <-schedulerDone:
		case <-time.After(drainTimeout):
			log.Warn("scheduler did not drain in time", "timeout", drainTimeout.String())
		}
	}

	log.Info("shutting down", "timeout", shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out, closing", "error", err)
		_ = server.Close()
	}
	log.Info("shutdown complete")
	if failure {
		return 1
	}
	return 0
}

// checker renders PASS, FAIL and SKIP lines for status and doctor.
type checker struct {
	ctx    context.Context
	failed bool
}

func (c *checker) pass(name, detail string) {
	fmt.Printf("PASS %-10s %s\n", name, detail)
}

func (c *checker) fail(name string, err error) {
	c.failed = true
	fmt.Printf("FAIL %-10s %v\n", name, err)
}

func (c *checker) skip(name, detail string) {
	fmt.Printf("SKIP %-10s %s\n", name, detail)
}

// printRedacted prints the effective configuration with every secret masked.
func printRedacted(cfg config.Config) {
	for _, line := range cfg.Redacted() {
		fmt.Println(line)
	}
}

// runStatus prints the redacted configuration and the dependency
// reachability. It always exits 0 once the configuration loaded.
func runStatus(cfg config.Config, log *slog.Logger) int {
	printRedacted(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	c := &checker{ctx: ctx}
	c.postgres(cfg)
	c.workdir(cfg)
	c.ffmpegBuild(cfg, log)
	c.filehouse(cfg)
	c.teamusers(cfg)
	return 0
}

// runDoctor runs the same checks as status and exits non-zero when one fails.
func runDoctor(cfg config.Config, log *slog.Logger) int {
	printRedacted(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	c := &checker{ctx: ctx}
	c.postgres(cfg)
	c.workdir(cfg)
	c.ffmpegBuild(cfg, log)
	c.filehouse(cfg)
	c.teamusers(cfg)
	if c.failed {
		return 1
	}
	return 0
}

// postgres opens the pool, pings it and reports the server version.
func (c *checker) postgres(cfg config.Config) {
	if strings.TrimSpace(cfg.DSN) == "" {
		c.skip("postgres", config.EnvDSN+" is not configured")
		return
	}
	db, err := store.New(c.ctx, cfg.DSN)
	if err != nil {
		c.fail("postgres", err)
		return
	}
	defer db.Close()
	version := "unknown"
	if db.Pool != nil {
		if err := db.Pool.QueryRow(c.ctx, "SELECT current_setting('server_version')").Scan(&version); err != nil {
			version = "unknown (" + err.Error() + ")"
		}
	}
	c.pass("postgres", "connected, server_version "+version)
}

// workdir checks that the scratch directory exists or can be created, and that
// a file can actually be written inside it: the merged video and the extracted
// ffmpeg live there, so an unwritable path fails every job at the last step.
func (c *checker) workdir(cfg config.Config) {
	if strings.TrimSpace(cfg.WorkDir) == "" {
		c.skip("workdir", config.EnvWorkDir+" is not configured")
		return
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		c.fail("workdir", err)
		return
	}
	probe, err := os.CreateTemp(cfg.WorkDir, ".doctor-*")
	if err != nil {
		c.fail("workdir", err)
		return
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	c.pass("workdir", "writable: "+cfg.WorkDir)
}

// ffmpegBuild resolves the ffmpeg this process would run and checks that it
// provides every demuxer the supported codecs need. A build missing one of them
// would advertise a codec it cannot merge, so that is a failure, not a warning.
func (c *checker) ffmpegBuild(cfg config.Config, log *slog.Logger) {
	binary, err := ffmpeg.New(ffmpeg.Options{
		BinaryPath: cfg.FFmpegPath,
		WorkDir:    cfg.WorkDir,
		Logger:     log,
	})
	if err != nil {
		c.fail("ffmpeg", err)
		return
	}
	path, err := binary.Path(c.ctx)
	if err != nil {
		c.fail("ffmpeg", err)
		return
	}
	version, err := binary.Version(c.ctx)
	if err != nil {
		c.fail("ffmpeg", err)
		return
	}
	demuxers, err := binary.Demuxers(c.ctx)
	if err != nil {
		c.fail("ffmpeg", err)
		return
	}
	var missing []string
	for _, codec := range ffmpeg.SupportedCodecs() {
		format, ok := ffmpeg.FormatFor(codec, false)
		if !ok {
			continue
		}
		if !demuxers[format.Demuxer] {
			missing = append(missing, format.Demuxer)
		}
	}
	source := "embedded"
	if strings.TrimSpace(cfg.FFmpegPath) != "" {
		source = config.EnvFFmpegPath
	}
	if len(missing) > 0 {
		c.fail("ffmpeg", fmt.Errorf("%s (%s, version %s) lacks the demuxers %s",
			path, source, version, strings.Join(missing, ", ")))
		return
	}
	c.pass("ffmpeg", fmt.Sprintf("version %s (%s), demuxers %s present", version, source, demuxerList()))
}

// demuxerList names every demuxer the supported codecs require, for PASS lines.
func demuxerList() string {
	var names []string
	for _, codec := range ffmpeg.SupportedCodecs() {
		if format, ok := ffmpeg.FormatFor(codec, false); ok {
			names = append(names, format.Demuxer)
		}
	}
	return strings.Join(names, ", ")
}

// filehouse probes filehouse's public health endpoint. A token is not needed:
// the probe is public, like the one the supervisor uses.
func (c *checker) filehouse(cfg config.Config) {
	url := strings.TrimSuffix(cfg.FilehouseURL, "/")
	if url == "" {
		c.skip("filehouse", config.EnvFilehouseURL+" is not configured")
		return
	}
	if err := c.probe(url + "/healthz"); err != nil {
		c.fail("filehouse", err)
		return
	}
	c.pass("filehouse", "reachable: "+cfg.FilehouseURL)
}

// teamusers probes the JWKS document this service verifies tokens against, so a
// wrong base URL or an unreachable issuer is caught before the first request.
func (c *checker) teamusers(cfg config.Config) {
	url := strings.TrimSuffix(cfg.TeamusersURL, "/")
	if url == "" {
		c.skip("teamusers", config.EnvTeamusersURL+" is not configured")
		return
	}
	if err := c.probe(url + "/.well-known/jwks.json"); err != nil {
		c.fail("teamusers", err)
		return
	}
	c.pass("teamusers", "JWKS reachable: "+cfg.TeamusersURL)
}

// probe issues one GET under the checker context and reports a non-success
// status as an error.
func (c *checker) probe(url string) error {
	ctx, cancel := context.WithTimeout(c.ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
	}
	return nil
}

// runRegisterPermissions upserts the merge:* catalog in teamusers. The teamusers
// base URL is guaranteed by validateCommandConfig.
func runRegisterPermissions(cfg config.Config, log *slog.Logger, token string) int {
	if strings.TrimSpace(token) == "" {
		log.Error("invalid configuration", "error", "an admin bearer token is required (-token or "+config.EnvTeamusersAdminToken+")")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
	defer cancel()
	auth, err := iamauth.New(iamauthOptions(cfg, log))
	if err != nil {
		log.Error("init iam authorizer", "error", err)
		return 1
	}
	defer auth.Close()
	if err := auth.RegisterPermissions(ctx, token, iamauth.PermissionKeys); err != nil {
		log.Error("register permissions", "error", err)
		return 1
	}
	for _, key := range iamauth.PermissionKeys {
		fmt.Println(key)
	}
	return 0
}

// runMigrate applies the embedded migrations and exits.
func runMigrate(cfg config.Config, log *slog.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.New(ctx, cfg.DSN)
	if err != nil {
		log.Error("open database", "error", err)
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Error("apply migrations", "error", err)
		return 1
	}
	log.Info("migrations applied", "version", version)
	return 0
}
