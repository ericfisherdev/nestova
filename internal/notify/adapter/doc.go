// Package adapter contains the notify context's outbound adapters: the
// Postgres-backed OutboxRepository (domain.Outbox) and the InAppSender
// (domain.Sender). Both are constructed with injected dependencies and satisfy
// their respective port interfaces at compile time.
package adapter
