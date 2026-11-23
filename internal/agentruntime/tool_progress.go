package agentruntime

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

const (
	maxToolCycleLength   = 3
	repeatedToolCycleCap = 4
)

var ErrRepeatedToolCycle = errors.New("Agent repeated a tool cycle without new results")

// checkToolProgress uses the raw Eino history at the next model boundary. Only
// consecutive completed tool batches count. New arguments or observations
// allow continued work, regardless of how long it has been running.
func checkToolProgress(input []*schema.Message) error {
	var batches [maxToolCycleLength * repeatedToolCycleCap][sha256.Size]byte
	count := 0
	end := len(input)
	for count < len(batches) {
		start := end
		for start > 0 && input[start-1] != nil && input[start-1].Role == schema.Tool {
			start--
		}
		if start == end || start == 0 {
			break
		}
		digest, ok := toolBatchDigest(input[start-1], input[start:end])
		if !ok {
			break
		}
		batches[count] = digest
		count++
		end = start - 1
	}
	for period := 1; period <= maxToolCycleLength && period*repeatedToolCycleCap <= count; period++ {
		if repeatsToolCycle(batches[:period*repeatedToolCycleCap], period) {
			return fmt.Errorf("%w (%d repetitions of %d tool batches)", ErrRepeatedToolCycle, repeatedToolCycleCap, period)
		}
	}
	return nil
}

func repeatsToolCycle(batches [][sha256.Size]byte, period int) bool {
	for i := period; i < len(batches); i++ {
		if batches[i] != batches[i%period] {
			return false
		}
	}
	return true
}

type toolObservation struct {
	Name      string
	Arguments string
	Result    string
}

func toolBatchDigest(request *schema.Message, results []*schema.Message) ([sha256.Size]byte, bool) {
	if request == nil || request.Role != schema.Assistant || len(request.ToolCalls) != len(results) {
		return [sha256.Size]byte{}, false
	}
	observations := make([]toolObservation, len(results))
	for i, call := range request.ToolCalls {
		// Waiting for another Agent is a synchronization boundary. A requested
		// wait timeout can return the same state while that Agent progresses.
		if call.Function.Name == "agent_wait" || results[i].ToolCallID != call.ID {
			return [sha256.Size]byte{}, false
		}
		observations[i] = toolObservation{
			Name: call.Function.Name, Arguments: canonicalToolArguments(call.Function.Arguments), Result: results[i].Content,
		}
	}
	encoded, err := json.Marshal(observations)
	return sha256.Sum256(encoded), err == nil
}

func canonicalToolArguments(arguments string) string {
	if !json.Valid([]byte(arguments)) {
		return arguments
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return arguments
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return arguments
	}
	return string(canonical)
}
