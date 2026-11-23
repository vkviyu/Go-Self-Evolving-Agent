package session

import "time"

// ChatHistory describes an existing root conversation. Workspace can be empty
// when the runtime did not record enough metadata to identify it.
type ChatHistory struct {
	ID        ID
	Prompt    string
	Workspace string
	CreatedAt time.Time
	UpdatedAt time.Time
}
