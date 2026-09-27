package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	S3Bucket        string
	S3Region        string
	S3Endpoint      string
	S3AccessKey     string
	S3SecretKey     string
	S3ObjectPrefix  string
	TUSBasePath     string
	TUSMaxSize      int64
	ServerAddr      string
	PublicURL       string
	RateLimitGlobal int
	RateLimitPerIP  int
	LogLevel        string

	// TrustedProxies lists the peers whose X-Forwarded-For header is
	// believed when determining the client IP (e.g. the local Caddy).
	TrustedProxies []netip.Prefix

	// MCP request limits: body size and concurrent POSTs.
	MCPMaxBodyBytes    int64
	MCPRateLimitGlobal int
	MCPRateLimitPerIP  int

	// IncompleteUploadTTL is how long an unfinished upload may sit idle
	// before the expiry worker removes it and aborts its multipart upload.
	IncompleteUploadTTL time.Duration
}

func Load() *Config {
	return &Config{
		S3Bucket:        mustEnv("S3_BUCKET"),
		S3Region:        mustEnv("S3_REGION"),
		S3Endpoint:      mustEnv("S3_ENDPOINT"),
		S3AccessKey:     mustEnv("S3_ACCESS_KEY"),
		S3SecretKey:     mustEnv("S3_SECRET_KEY"),
		S3ObjectPrefix:  getEnvOrDefault("S3_OBJECT_PREFIX", "uploads/"),
		TUSBasePath:     getEnvOrDefault("TUS_BASE_PATH", "/files/"),
		TUSMaxSize:      mustEnvInt64("TUS_MAX_SIZE", 10737418240),
		ServerAddr:      getEnvOrDefault("SERVER_ADDR", ":8080"),
		PublicURL:       getEnvOrDefault("PUBLIC_URL", "http://localhost:8080"),
		RateLimitGlobal: mustEnvInt("RATE_LIMIT_GLOBAL", 50),
		RateLimitPerIP:  mustEnvInt("RATE_LIMIT_PER_IP", 5),
		LogLevel:        getEnvOrDefault("LOG_LEVEL", "info"),

		TrustedProxies:      mustPrefixes("TRUSTED_PROXIES", "127.0.0.1/32,::1/128"),
		MCPMaxBodyBytes:     mustEnvInt64("MCP_MAX_BODY_BYTES", 16<<20),
		MCPRateLimitGlobal:  mustEnvInt("MCP_RATE_LIMIT_GLOBAL", 10),
		MCPRateLimitPerIP:   mustEnvInt("MCP_RATE_LIMIT_PER_IP", 2),
		IncompleteUploadTTL: mustDuration("INCOMPLETE_UPLOAD_TTL", 48*time.Hour),
	}
}

// mustPrefixes parses a comma-separated list of IPs or CIDRs. An empty
// value (explicitly set) trusts no proxies.
func mustPrefixes(key, def string) []netip.Prefix {
	v, ok := os.LookupEnv(key)
	if !ok {
		v = def
	}
	var out []netip.Prefix
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			addr, err := netip.ParseAddr(f)
			if err != nil {
				panic(fmt.Sprintf("invalid value for %s: %v", key, err))
			}
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			panic(fmt.Sprintf("invalid value for %s: %v", key, err))
		}
		out = append(out, p.Masked())
	}
	return out
}

func mustDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		panic(fmt.Sprintf("invalid value for %s: %q", key, v))
	}
	return d
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("required environment variable %s is not set", key))
	}
	return v
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustEnvInt64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		panic(fmt.Sprintf("invalid value for %s: %v", key, err))
	}
	return n
}

func mustEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		panic(fmt.Sprintf("invalid value for %s: %v", key, err))
	}
	return n
}
