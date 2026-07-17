import Darwin
import Foundation

private let requiredCapacityBytes: Int64 = 17_179_869_184
private let fixedError = "error: Darwin capacity observation unavailable\n"

@inline(__always)
private func fail() -> Never {
    FileHandle.standardError.write(Data(fixedError.utf8))
    exit(2)
}

private func exactBytes(_ value: Int?) -> Int64? {
    guard let value, value >= 0 else { return nil }
    return Int64(value)
}

private func exactBytes(_ value: Int64?) -> Int64? {
    guard let value, value >= 0 else { return nil }
    return value
}

let arguments = CommandLine.arguments
guard arguments.count == 2, arguments[1].hasPrefix("/") else { fail() }

let volumeURL = URL(fileURLWithPath: arguments[1], isDirectory: true).standardizedFileURL
let keys: Set<URLResourceKey> = [
    .volumeTotalCapacityKey,
    .volumeAvailableCapacityKey,
    .volumeAvailableCapacityForImportantUsageKey,
    .volumeAvailableCapacityForOpportunisticUsageKey,
]

do {
    let values = try volumeURL.resourceValues(forKeys: keys)
    guard
        let total = exactBytes(values.volumeTotalCapacity),
        let immediatelyAvailable = exactBytes(values.volumeAvailableCapacity),
        let importantUsage = exactBytes(values.volumeAvailableCapacityForImportantUsage),
        let opportunisticUsage = exactBytes(values.volumeAvailableCapacityForOpportunisticUsage),
        total > 0,
        immediatelyAvailable <= total,
        importantUsage <= total,
        opportunisticUsage <= total
    else { fail() }

    let importantUsageMeetsThreshold = importantUsage >= requiredCapacityBytes
    let rawFreeMeetsThreshold = immediatelyAvailable >= requiredCapacityBytes
    let thresholdStatus = importantUsageMeetsThreshold
        ? "important_usage_meets_threshold_unattested"
        : "important_usage_below_threshold_unattested"

    let observation: [String: Any] = [
        "authority": [
            "benchmark_execution": "forbidden_pending_separate_owner_approval",
            "candidate_execution": "forbidden_plan_unexecutable",
            "model_provider_execution": "forbidden_not_authorized",
            "paid_execution": "forbidden_not_authorized",
        ],
        "execution_status": "forbidden_missing_trusted_observer_and_atomic_reservation",
        "measurement": [
            "available_capacity_bytes": immediatelyAvailable,
            "available_capacity_for_important_usage_bytes": importantUsage,
            "available_capacity_for_opportunistic_usage_bytes": opportunisticUsage,
            "important_usage_threshold_met": importantUsageMeetsThreshold,
            "measurement_api": "Foundation.URLResourceValues.volume_capacity_v1",
            "raw_free_threshold_met": rawFreeMeetsThreshold,
            "required_capacity_bytes": requiredCapacityBytes,
            "threshold_status": thresholdStatus,
            "total_capacity_bytes": total,
        ],
        "profile": "agent_brain_negative_control_darwin_capacity_observation_v1",
        "reservation": [
            "cleanup_attestation_sha256": NSNull(),
            "reserved_bytes": NSNull(),
            "status": "absent_not_implemented",
            "trusted_observer_attestation_sha256": NSNull(),
        ],
        "residual_gates": [
            "trusted_darwin_capacity_observer_attestation",
            "bounded_atomic_resource_reservation_and_cleanup_attestation",
        ],
        "schema_version": 1,
        "status": "live_unattested_capacity_observation_execution_forbidden",
    ]

    guard JSONSerialization.isValidJSONObject(observation) else { fail() }
    var output = try JSONSerialization.data(withJSONObject: observation, options: [.sortedKeys])
    output.append(0x0a)
    FileHandle.standardOutput.write(output)
} catch {
    fail()
}
