package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultProvider = "openai"
	defaultBaseURL  = "https://api.deepseek.com/v1"
	defaultModel    = "deepseek-flash"
)

// Load local settings without overriding explicitly exported environment values.
// run_backend.sh starts in backend/, so ../.env is the project-local file.
func loadLocalConfig() error {
	for _, path := range []string{".env", "../.env"} {
		if err := loadEnvFile(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		break
	}
	return nil
}

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		entry := strings.TrimSpace(scanner.Text())
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		key, value, found := strings.Cut(strings.TrimPrefix(entry, "export "), "=")
		if !found {
			return fmt.Errorf("invalid local configuration at %s:%d", path, line)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "LLM_PROVIDER", "LLM_BASE_URL", "LLM_MODEL", "LLM_API_KEY", "MINICOCO_HOST", "MINICOCO_PORT", "MINICOCO_DATA_DIR", "LLM_MOCK_MODE", "LLM_MOCK_FLAKY_FAILS", "STORE_BACKEND", "MONGO_URI", "MONGO_DATABASE", "REDIS_URL", "REDIS_PREFIX", "MINICOCO_MODEL_IS_FUNCTION_CALL", "MINICOCO_DEBUG_PROMPT":
		default:
			continue
		}
		if strings.HasPrefix(value, "\"") {
			value, err = strconv.Unquote(value)
			if err != nil {
				return fmt.Errorf("invalid quoted local configuration at %s:%d", path, line)
			}
		} else if strings.HasPrefix(value, "'") {
			if len(value) < 2 || !strings.HasSuffix(value, "'") {
				return fmt.Errorf("invalid quoted local configuration at %s:%d", path, line)
			}
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("could not apply local configuration for %s", key)
			}
		}
	}
	return scanner.Err()
}
