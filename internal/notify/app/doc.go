// Package app contains the notify context's application service: the
// Dispatcher, which drains the notification outbox and delivers notifications
// through channel-specific Sender implementations.
//
// # Outbox Pattern
//
// The notification outbox decouples the act of scheduling a notification from
// the act of delivering it. Producers (other bounded contexts) write a
// Notification row to the notification table in the same database transaction
// as their own domain change, guaranteeing the notification is never lost even
// if the process crashes before delivery. The Dispatcher runs as a background
// goroutine, polling the outbox and delivering due notifications.
//
// # Claim / Send / Mark Lifecycle
//
// 1. Enqueue: a producer calls Outbox.Enqueue, creating a row with
// status='pending' and a scheduled_for timestamp.
//
// 2. Claim: Dispatcher.RunOnce calls Outbox.ClaimDue, which atomically
// selects due pending rows with FOR UPDATE SKIP LOCKED and transitions them
// to status='sent' (leaving sent_at NULL) in a single UPDATE statement
// (optimistic claim). This ensures concurrent dispatchers never claim the same
// row, and releases the row lock immediately — no transaction is held open
// during the send call.
//
// 3. Send: the Dispatcher invokes the appropriate Sender for each claimed
// notification. On success, MarkSent stamps sent_at with the actual delivery
// time (the row was already 'sent' from step 2). On failure, MarkFailed
// downgrades the row to status='failed'.
//
// 4. Recovery (future): because a claimed row has (status='sent', sent_at IS
// NULL) until delivery stamps sent_at, a periodic sweep can detect rows stuck
// in that state past a threshold — claimed but never delivered — and retry or
// alert on them.
//
// # Delivery Semantics
//
// The optimistic-claim design is effectively at-most-once: a crash after
// ClaimDue succeeds but before the Sender runs leaves the row in
// (status='sent', sent_at IS NULL) and this skeleton never retries it, so the
// notification is silently dropped. That limbo state is deliberately
// detectable (see Recovery) so the gap can be closed later. This is an
// acceptable trade-off for a skeleton outbox.
package app
