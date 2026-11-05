package main

import (
	"encoding/json"

	"github.com/vkviyu/go-self-evolving-agent/sdk/pluginabi"
)

func init() {
	pluginabi.Register(func(request json.RawMessage) (json.RawMessage, error) {
		var command struct {
			Loop bool `json:"loop"`
		}
		_ = json.Unmarshal(request, &command)
		if command.Loop {
			for {
			}
		}
		return request, nil
	})
}

func main() {}
