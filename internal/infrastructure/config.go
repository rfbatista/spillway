package infrastructure

import (
	"os"
)

type Config struct {
	Port string `json:"port,omitempty"`
}

func NewConfig() Config {
	return Config{Port: os.Getenv("PORT")}
}
