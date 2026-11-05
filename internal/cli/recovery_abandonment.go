package cli

import (
	"encoding/json"
	"io"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func writeAbandonment(writer io.Writer, receipt session.Abandonment) error {
	return json.NewEncoder(writer).Encode(receipt)
}
