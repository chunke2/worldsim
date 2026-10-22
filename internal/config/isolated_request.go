package config

import "errors"

// ValidateIsolatedExtraBody preserves vendor generation options while refusing
// fields that can replace the request or attach server-side shared history.
// This validates at call time; it never rewrites the user's configuration.
func ValidateIsolatedExtraBody(extra map[string]any) error {
	protected := map[string]bool{
		"model": true, "messages": true, "stream": true, "max_tokens": true, "max_completion_tokens": true, "temperature": true,
		"tools": true, "tool_choice": true, "functions": true, "function_call": true,
		"conversation": true, "conversation_id": true, "thread_id": true, "session_id": true, "previous_response_id": true,
		"history": true, "input": true, "prompt": true, "system": true, "instructions": true, "assistant_id": true, "memory": true, "file_ids": true, "attachments": true,
	}
	for key := range extra {
		if protected[key] {
			return errors.New("extra_body cannot override isolated request fields or attach shared conversation history")
		}
	}
	return nil
}
