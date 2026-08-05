// util/util.go

package util

import (
	"log"
	"os"
	"strconv"
)

func MustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("%s is not set in environment", key)
	}

	return val
}

func EnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
