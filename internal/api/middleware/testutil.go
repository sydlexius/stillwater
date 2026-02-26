package middleware

import "context"

// WithTestUserID injects a user ID into the context. This is intended for
// handler-level unit tests that call handler methods directly (bypassing the
// auth middleware). Production code should rely on the Auth or OptionalAuth
// middleware to populate this value.
func WithTestUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}
