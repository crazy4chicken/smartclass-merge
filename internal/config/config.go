// Package config loads the smartclass-merge configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variables read by Load and printed by Redacted.
const (
	EnvListenAddr            = "MERGE_LISTEN_ADDR"
	EnvDSN                   = "MERGE_DSN"
	EnvTeamusersURL          = "MERGE_TEAMUSERS_URL"
	EnvTeamusersIssuer       = "MERGE_TEAMUSERS_ISSUER"
	EnvTeamusersAud          = "MERGE_TEAMUSERS_AUD"
	EnvTeamusersClientID     = "MERGE_TEAMUSERS_CLIENT_ID"
	EnvTeamusersClientSecret = "MERGE_TEAMUSERS_CLIENT_SECRET"
	EnvTeamusersAdminToken   = "MERGE_TEAMUSERS_ADMIN_TOKEN"
	EnvTeamusersTimeout      = "MERGE_TEAMUSERS_TIMEOUT"
	EnvFilehouseURL          = "MERGE_FILEHOUSE_URL"
	EnvFilehouseTimeout      = "MERGE_FILEHOUSE_TIMEOUT"
	EnvSourceBucket          = "MERGE_SOURCE_BUCKET"
	EnvDestBucket            = "MERGE_DEST_BUCKET"
	EnvObjectMaxBytes        = "MERGE_OBJECT_MAX_BYTES"
	EnvMultipartPartBytes    = "MERGE_MULTIPART_PART_BYTES"
	EnvDefaultFPS            = "MERGE_DEFAULT_FPS"
	EnvWorkDir               = "MERGE_WORK_DIR"
	EnvFFmpegPath            = "MERGE_FFMPEG_PATH"
	EnvMaxConcurrentJobs     = "MERGE_MAX_CONCURRENT_JOBS"
	EnvSchedTick             = "MERGE_SCHED_TICK"
	EnvJobLease              = "MERGE_JOB_LEASE"
	EnvMaxAttempts           = "MERGE_MAX_ATTEMPTS"
	EnvRetryBackoff          = "MERGE_RETRY_BACKOFF"
	EnvJobTimeout            = "MERGE_JOB_TIMEOUT"
	EnvTranscodeFallback     = "MERGE_TRANSCODE_FALLBACK"
	EnvDev                   = "MERGE_DEV"
	EnvLogLevel              = "MERGE_LOG_LEVEL"
	EnvFile                  = "MERGE_ENV_FILE"
)

// MinFPS and MaxFPS bound both the configured default frame rate and the fps a
// merge request may declare: the raw bitstream carries no timing, so the
// requester's fps is the only source of playback speed.
const (
	MinFPS = 1
	MaxFPS = 240
)

// Defaults applied by FromEnv when a variable is unset or empty.
const (
	defaultListenAddr         = ":8083"
	defaultTeamusersURL       = "http://127.0.0.1:8082"
	defaultTeamusersIssuer    = "teamusers"
	defaultTeamusersAudience  = "teamusers"
	defaultTeamusersTimeout   = 5 * time.Second
	defaultFilehouseTimeout   = 30 * time.Second
	defaultSourceBucket       = "webcam-segments"
	defaultObjectMaxBytes     = 5 * 1024 * 1024 * 1024
	defaultMultipartPartBytes = 64 * 1024 * 1024
	defaultFPS                = 25
	defaultMaxConcurrentJobs  = 1
	defaultSchedTick          = 5 * time.Second
	defaultJobLease           = 2 * time.Minute
	defaultMaxAttempts        = 3
	defaultRetryBackoff       = time.Minute
	defaultJobTimeout         = 6 * time.Hour
	defaultLogLevel           = "info"
)

// maxMultipartPartBytes is the largest multipart part filehouse accepts.
const maxMultipartPartBytes = 256 * 1024 * 1024

// Config holds all smartclass-merge configuration.
type Config struct {
	ListenAddr            string        // MERGE_LISTEN_ADDR, default ":8083"
	DSN                   string        // MERGE_DSN (required)
	TeamusersURL          string        // MERGE_TEAMUSERS_URL, default "http://127.0.0.1:8082"
	TeamusersIssuer       string        // MERGE_TEAMUSERS_ISSUER, default "teamusers"
	TeamusersAudience     string        // MERGE_TEAMUSERS_AUD, default "teamusers"
	TeamusersClientID     string        // MERGE_TEAMUSERS_CLIENT_ID (required unless Dev)
	TeamusersClientSecret string        // MERGE_TEAMUSERS_CLIENT_SECRET (required unless Dev)
	TeamusersAdminToken   string        // MERGE_TEAMUSERS_ADMIN_TOKEN (used by register-permissions)
	TeamusersTimeout      time.Duration // MERGE_TEAMUSERS_TIMEOUT, default 5s
	FilehouseURL          string        // MERGE_FILEHOUSE_URL (required unless Dev)
	FilehouseTimeout      time.Duration // MERGE_FILEHOUSE_TIMEOUT, default 30s
	SourceBucket          string        // MERGE_SOURCE_BUCKET, default "webcam-segments"
	DestBucket            string        // MERGE_DEST_BUCKET (required)
	ObjectMaxBytes        int64         // MERGE_OBJECT_MAX_BYTES, default 5 GiB
	MultipartPartBytes    int64         // MERGE_MULTIPART_PART_BYTES, default 64 MiB, at most 256 MiB
	DefaultFPS            int           // MERGE_DEFAULT_FPS, default 25
	WorkDir               string        // MERGE_WORK_DIR, default <os temp dir>/smartclass-merge
	FFmpegPath            string        // MERGE_FFMPEG_PATH, empty means the embedded binary
	MaxConcurrentJobs     int           // MERGE_MAX_CONCURRENT_JOBS, default 1
	SchedTick             time.Duration // MERGE_SCHED_TICK, default 5s
	JobLease              time.Duration // MERGE_JOB_LEASE, default 2m
	MaxAttempts           int           // MERGE_MAX_ATTEMPTS, default 3
	RetryBackoff          time.Duration // MERGE_RETRY_BACKOFF, default 1m
	JobTimeout            time.Duration // MERGE_JOB_TIMEOUT, default 6h
	TranscodeFallback     bool          // MERGE_TRANSCODE_FALLBACK, default true
	Dev                   bool          // MERGE_DEV, default false
	LogLevel              string        // MERGE_LOG_LEVEL, default "info"
}

// Load reads the configuration: MERGE_ENV_FILE, when set, is read first,
// then real environment variables win over the file, and defaults fill the
// rest. The result is checked with Validate, so a nil error means every value
// that is set is usable; the endpoints, credentials and buckets a subcommand
// needs are enforced separately by ValidateServer, ValidateDatabase and
// ValidateTeamusers.
func Load() (Config, error) {
	if path := strings.TrimSpace(os.Getenv(EnvFile)); path != "" {
		if err := loadEnvFile(path); err != nil {
			return Config{}, err
		}
	}
	cfg := Config{
		ListenAddr:            envOr(EnvListenAddr, defaultListenAddr),
		DSN:                   strings.TrimSpace(os.Getenv(EnvDSN)),
		TeamusersURL:          envOr(EnvTeamusersURL, defaultTeamusersURL),
		TeamusersIssuer:       envOr(EnvTeamusersIssuer, defaultTeamusersIssuer),
		TeamusersAudience:     envOr(EnvTeamusersAud, defaultTeamusersAudience),
		TeamusersClientID:     os.Getenv(EnvTeamusersClientID),
		TeamusersClientSecret: os.Getenv(EnvTeamusersClientSecret),
		TeamusersAdminToken:   os.Getenv(EnvTeamusersAdminToken),
		FilehouseURL:          envOr(EnvFilehouseURL, ""),
		SourceBucket:          envOr(EnvSourceBucket, defaultSourceBucket),
		DestBucket:            envOr(EnvDestBucket, ""),
		WorkDir:               envOr(EnvWorkDir, defaultWorkDir()),
		FFmpegPath:            envOr(EnvFFmpegPath, ""),
		LogLevel:              envOr(EnvLogLevel, defaultLogLevel),
	}
	var err error
	if cfg.TeamusersTimeout, err = envDuration(EnvTeamusersTimeout, defaultTeamusersTimeout); err != nil {
		return Config{}, err
	}
	if cfg.FilehouseTimeout, err = envDuration(EnvFilehouseTimeout, defaultFilehouseTimeout); err != nil {
		return Config{}, err
	}
	if cfg.SchedTick, err = envDuration(EnvSchedTick, defaultSchedTick); err != nil {
		return Config{}, err
	}
	if cfg.JobLease, err = envDuration(EnvJobLease, defaultJobLease); err != nil {
		return Config{}, err
	}
	if cfg.RetryBackoff, err = envDuration(EnvRetryBackoff, defaultRetryBackoff); err != nil {
		return Config{}, err
	}
	if cfg.JobTimeout, err = envDuration(EnvJobTimeout, defaultJobTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ObjectMaxBytes, err = envInt64(EnvObjectMaxBytes, defaultObjectMaxBytes); err != nil {
		return Config{}, err
	}
	if cfg.MultipartPartBytes, err = envInt64(EnvMultipartPartBytes, defaultMultipartPartBytes); err != nil {
		return Config{}, err
	}
	if cfg.DefaultFPS, err = envInt(EnvDefaultFPS, defaultFPS); err != nil {
		return Config{}, err
	}
	if cfg.MaxConcurrentJobs, err = envInt(EnvMaxConcurrentJobs, defaultMaxConcurrentJobs); err != nil {
		return Config{}, err
	}
	if cfg.MaxAttempts, err = envInt(EnvMaxAttempts, defaultMaxAttempts); err != nil {
		return Config{}, err
	}
	if cfg.TranscodeFallback, err = envBool(EnvTranscodeFallback, true); err != nil {
		return Config{}, err
	}
	if cfg.Dev, err = envBool(EnvDev, false); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports every malformed value: an unknown log level, a frame rate
// outside MinFPS..MaxFPS and nonsensical duration or count. It requires no
// endpoint, credential or bucket, because each subcommand needs a different
// subset of them: ValidateServer, ValidateDatabase and ValidateTeamusers add
// those presence checks.
func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.ListenAddr) == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", EnvListenAddr))
	}
	if strings.TrimSpace(c.TeamusersIssuer) == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", EnvTeamusersIssuer))
	}
	if strings.TrimSpace(c.TeamusersAudience) == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", EnvTeamusersAud))
	}
	if c.TeamusersTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvTeamusersTimeout))
	}
	if c.FilehouseTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvFilehouseTimeout))
	}
	if c.SchedTick <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvSchedTick))
	}
	if c.JobLease <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvJobLease))
	}
	if c.RetryBackoff <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvRetryBackoff))
	}
	if c.JobTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvJobTimeout))
	}
	if c.ObjectMaxBytes <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvObjectMaxBytes))
	}
	if c.MultipartPartBytes <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvMultipartPartBytes))
	}
	if c.MultipartPartBytes > maxMultipartPartBytes {
		errs = append(errs, fmt.Errorf("%s must not exceed %d bytes (256 MiB)", EnvMultipartPartBytes, maxMultipartPartBytes))
	}
	if c.DefaultFPS < MinFPS || c.DefaultFPS > MaxFPS {
		errs = append(errs, fmt.Errorf("%s must be between %d and %d", EnvDefaultFPS, MinFPS, MaxFPS))
	}
	if strings.TrimSpace(c.WorkDir) == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", EnvWorkDir))
	}
	if c.MaxConcurrentJobs <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvMaxConcurrentJobs))
	}
	if c.MaxAttempts <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvMaxAttempts))
	}
	if _, ok := parseLevel(c.LogLevel); !ok {
		errs = append(errs, fmt.Errorf("%s: unknown level %q", EnvLogLevel, c.LogLevel))
	}
	return errors.Join(errs...)
}

// ValidateServer reports what `run` cannot start without: the parse-level
// checks plus the database DSN, the destination bucket and, unless Dev is set,
// the filehouse endpoint and the teamusers client credentials. Dev waives the
// endpoints and credentials, mirroring the fleet's dev bypass, but never the
// DSN or the destination bucket: a job without a bucket to write to is not a
// degraded run, it is a lost recording.
func (c Config) ValidateServer() error {
	errs := []error{c.Validate()}
	if strings.TrimSpace(c.DSN) == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvDSN))
	}
	if strings.TrimSpace(c.DestBucket) == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvDestBucket))
	}
	if !c.Dev {
		if strings.TrimSpace(c.FilehouseURL) == "" {
			errs = append(errs, fmt.Errorf("%s is required (or set %s=true)", EnvFilehouseURL, EnvDev))
		}
		if strings.TrimSpace(c.TeamusersClientID) == "" {
			errs = append(errs, fmt.Errorf("%s is required (or set %s=true)", EnvTeamusersClientID, EnvDev))
		}
		if strings.TrimSpace(c.TeamusersClientSecret) == "" {
			errs = append(errs, fmt.Errorf("%s is required (or set %s=true)", EnvTeamusersClientSecret, EnvDev))
		}
	}
	return errors.Join(errs...)
}

// ValidateDatabase reports what `migrate` cannot run without: the parse-level
// checks plus the DSN. Migrating touches the database alone, so the endpoints,
// credentials and buckets the server needs are irrelevant.
func (c Config) ValidateDatabase() error {
	errs := []error{c.Validate()}
	if strings.TrimSpace(c.DSN) == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvDSN))
	}
	return errors.Join(errs...)
}

// ValidateTeamusers reports what `register-permissions` cannot run without:
// the parse-level checks plus the teamusers base URL. Registration
// authenticates with an admin bearer token (-token or MERGE_TEAMUSERS_ADMIN_TOKEN),
// which the subcommand checks itself, so the service client credentials are not
// required.
func (c Config) ValidateTeamusers() error {
	errs := []error{c.Validate()}
	if strings.TrimSpace(c.TeamusersURL) == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvTeamusersURL))
	}
	return errors.Join(errs...)
}

// Redacted renders the effective configuration as KEY=value lines with every
// secret masked. It is safe to print.
func (c Config) Redacted() []string {
	return []string{
		EnvListenAddr + "=" + c.ListenAddr,
		EnvDSN + "=" + redactDSN(c.DSN),
		EnvTeamusersURL + "=" + c.TeamusersURL,
		EnvTeamusersIssuer + "=" + c.TeamusersIssuer,
		EnvTeamusersAud + "=" + c.TeamusersAudience,
		EnvTeamusersClientID + "=" + c.TeamusersClientID,
		EnvTeamusersClientSecret + "=" + maskSecret(c.TeamusersClientSecret),
		EnvTeamusersAdminToken + "=" + maskSecret(c.TeamusersAdminToken),
		EnvTeamusersTimeout + "=" + c.TeamusersTimeout.String(),
		EnvFilehouseURL + "=" + c.FilehouseURL,
		EnvFilehouseTimeout + "=" + c.FilehouseTimeout.String(),
		EnvSourceBucket + "=" + c.SourceBucket,
		EnvDestBucket + "=" + c.DestBucket,
		EnvObjectMaxBytes + "=" + strconv.FormatInt(c.ObjectMaxBytes, 10),
		EnvMultipartPartBytes + "=" + strconv.FormatInt(c.MultipartPartBytes, 10),
		EnvDefaultFPS + "=" + strconv.Itoa(c.DefaultFPS),
		EnvWorkDir + "=" + c.WorkDir,
		EnvFFmpegPath + "=" + c.FFmpegPath,
		EnvMaxConcurrentJobs + "=" + strconv.Itoa(c.MaxConcurrentJobs),
		EnvSchedTick + "=" + c.SchedTick.String(),
		EnvJobLease + "=" + c.JobLease.String(),
		EnvMaxAttempts + "=" + strconv.Itoa(c.MaxAttempts),
		EnvRetryBackoff + "=" + c.RetryBackoff.String(),
		EnvJobTimeout + "=" + c.JobTimeout.String(),
		EnvTranscodeFallback + "=" + strconv.FormatBool(c.TranscodeFallback),
		EnvDev + "=" + strconv.FormatBool(c.Dev),
		EnvLogLevel + "=" + c.LogLevel,
	}
}

// Level returns the slog level for LogLevel, defaulting to info.
func (c Config) Level() slog.Level {
	level, _ := parseLevel(c.LogLevel)
	return level
}

// defaultWorkDir is where jobs keep their temp video before upload: a
// per-service directory under the OS temp dir, so a shared /tmp does not mix
// two services' scratch files.
func defaultWorkDir() string {
	return filepath.Join(os.TempDir(), "smartclass-merge")
}

func parseLevel(raw string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// loadEnvFile reads KEY=VALUE assignments from path into the process
// environment. Real environment variables always win, so values injected by
// systemd or compose are never overwritten by the file.
func loadEnvFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvFile, err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		key, value, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("%s: line %d: missing '='", path, i+1)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s: line %d: missing variable name", path, i+1)
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s: line %d: %w", path, i+1, err)
		}
	}
	return nil
}

// maskSecret hides a non-empty secret, and keeps an unset one visibly empty.
func maskSecret(value string) string {
	if value == "" {
		return ""
	}
	return "<redacted>"
}

// redactDSN masks the password of a PostgreSQL connection string in either URL
// or keyword/value form.
func redactDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "<redacted>"
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(parsed.User.Username(), "<redacted>")
			// url.URL escapes the mask; render it verbatim.
			return strings.ReplaceAll(parsed.String(), "%3Credacted%3E", "<redacted>")
		}
		return parsed.String()
	}
	fields := strings.Fields(dsn)
	for i, field := range fields {
		if strings.HasPrefix(strings.ToLower(field), "password=") {
			fields[i] = "password=<redacted>"
		}
	}
	return strings.Join(fields, " ")
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, raw)
	}
	return value, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, raw)
	}
	return value, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q", key, raw)
	}
	return value, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q", key, raw)
	}
	return value, nil
}
