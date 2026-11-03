// Package brainstore owns shared Brain storage primitives: process and file
// locks, scoped write locks, atomic file replacement, bounded
// artifact reads, and descriptor-pinned private SQLite snapshots.
//
// Callers retain responsibility for domain policy, path validation appropriate
// to each artifact, and publication transactions.
// The process lock registry and failure-injection hooks each have one home here.
// Snapshot readers copy and replay the source WAL in a private directory;
// callers own the returned cleanup and retain publication-marker and policy
// checks around snapshot acquisition.
package brainstore
