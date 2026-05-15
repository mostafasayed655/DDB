package common

import (
	"encoding/json"
	"fmt"
	"os"
)

func LoadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	return nil
}
