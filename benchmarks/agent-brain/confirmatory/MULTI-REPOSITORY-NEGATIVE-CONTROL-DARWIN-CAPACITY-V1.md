# Darwin capacity observation v1

This diagnostic closes one measurement ambiguity without authorizing execution. On APFS, `df /`
combines a shared container ceiling, sealed System-volume usage, and shared immediately unallocated
blocks. It does not report the same capacity macOS exposes to important work after reclaiming
purgeable data.

`negative_control_darwin_capacity_v1.swift` reads four documented Foundation volume-capacity values
for one caller-supplied absolute volume path:

- total capacity;
- immediately available capacity;
- capacity available for important usage;
- capacity available for opportunistic usage.

The path is never emitted. Output is compact, sorted-key UTF-8 JSON with one LF and validates against
`schemas/negative-control-darwin-capacity-observation-v1.schema.json`. The exact planning threshold is
16 GiB (`17,179,869,184` bytes). Both raw-free and important-usage comparisons are reported so one
cannot be silently substituted for the other.

The observation is deliberately `live_unattested_capacity_observation_execution_forbidden`. It has
no signing, subprocess, Git, Go, worktree, candidate, model, network, private-log, or reservation
surface. Even a passing important-usage comparison leaves execution forbidden because this slice
does not authenticate the observer, reserve capacity, enforce the staging ceiling, or attest cleanup.

On 2026-07-17, the same Foundation API reported about 191.9 GB available for important usage on the
Data volume, matching System Settings. That live value is not checked in because it changes over
time. The earlier report that treated roughly 13 GB of immediately unallocated blocks as total usable
capacity was incorrect and is withdrawn.

## Local diagnostic

Compile and observe the Data volume without writing an artifact into the repository:

```bash
/usr/bin/xcrun swiftc \
  benchmarks/agent-brain/confirmatory/negative_control_darwin_capacity_v1.swift \
  -o /tmp/negative-control-darwin-capacity-v1
/tmp/negative-control-darwin-capacity-v1 /System/Volumes/Data
```

Run the focused synthetic/live-observation checks from the confirmatory directory:

```bash
python3 -m unittest test_negative_control_darwin_capacity_v1.py
```

Before any scheduled attempt, a later gate must pin and authenticate the observer, atomically reserve
bounded staging capacity, enforce the 8 GiB staging ceiling plus 8 GiB reserve, and attest cleanup.
