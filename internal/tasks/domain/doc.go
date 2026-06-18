// Package domain holds entities, value objects, sentinel errors, and port
// interfaces for the tasks bounded context.
//
// The tasks context manages recurring household chores and maintenance items.
// Each [RecurringTask] is a template that carries a recurrence [Cadence]
// (shared-kernel value object from the household domain), a [Category], and a
// [RotationPolicy] that governs how assignments are made. The generator
// (NES-30) materialises [TaskInstance] rows ahead of time; the adapter (NES-29)
// persists them via the repository ports defined here.
//
// Domain errors are collected in sentinel vars and documented on the port
// methods that return them. No framework or infrastructure imports belong here.
package domain
