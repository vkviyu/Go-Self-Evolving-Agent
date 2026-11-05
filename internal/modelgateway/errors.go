package modelgateway

import "fmt"

// HTTPStatusError preserves provider status at the model boundary without
// coupling the runtime's recovery policy to a provider SDK.
type HTTPStatusError struct {
	Status  int
	Code    string
	Message string
}

func (err *HTTPStatusError) Error() string {
	if err == nil {
		return "model provider HTTP error"
	}
	if err.Code == "" {
		return fmt.Sprintf("model provider HTTP %d: %s", err.Status, err.Message)
	}
	return fmt.Sprintf("model provider HTTP %d (%s): %s", err.Status, err.Code, err.Message)
}

func (err *HTTPStatusError) StatusCode() int {
	if err == nil {
		return 0
	}
	return err.Status
}
