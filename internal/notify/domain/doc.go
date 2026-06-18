// Package domain holds the notify bounded context's domain model: the
// Notification aggregate, typed IDs, Channel and Status enums, sentinel errors,
// and the Outbox/Sender port interfaces.
//
// The notify context is cross-cutting — other bounded contexts (tasks, meals,
// calendar) enqueue notifications here; the Dispatcher in the app layer drains
// the outbox and delivers them through channel-specific Sender implementations.
package domain
