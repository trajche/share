package config

import (
	"fmt"
	"os"
	"strconv"
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
	}
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
