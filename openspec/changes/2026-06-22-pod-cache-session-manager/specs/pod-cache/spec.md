## ADDED Requirements

### Requirement: PodCache provides thread-safe in-memory caching with TTL

The `PodCache` struct SHALL map session IDs to pod IPs and names using a `sync.RWMutex`-protected map with per-entry TTL expiry.

#### Scenario: Set and Get within TTL
- **WHEN** `Set` is called with sessionID "inv-1", podIP "10.0.0.1", podName "pod-abc"
- **AND** `Get` is called for "inv-1" within the TTL window
- **THEN** `Get` SHALL return podIP "10.0.0.1", podName "pod-abc", and ok=true

#### Scenario: Get returns miss for unknown session
- **WHEN** `Get` is called for a session ID that was never stored
- **THEN** it SHALL return empty strings and ok=false

#### Scenario: Get returns miss after TTL expiry
- **WHEN** `Set` is called and the TTL duration elapses
- **AND** `Get` is called for the same session ID
- **THEN** it SHALL return empty strings and ok=false

#### Scenario: Delete removes entry unconditionally
- **WHEN** `Delete` is called with a session ID
- **THEN** subsequent `Get` calls for that session SHALL return ok=false

#### Scenario: Invalidate removes entry only if pod IP matches
- **WHEN** `Invalidate` is called with sessionID and podIP
- **AND** the cached entry has the same podIP
- **THEN** the entry SHALL be removed

#### Scenario: Invalidate preserves entry if pod IP differs
- **WHEN** `Invalidate` is called with sessionID and a podIP different from the cached entry
- **THEN** the cached entry SHALL NOT be removed

#### Scenario: EvictExpired removes all expired entries
- **WHEN** `EvictExpired` is called
- **THEN** all entries whose TTL has expired SHALL be removed
- **AND** entries whose TTL has not expired SHALL be preserved
- **AND** the count of evicted entries SHALL be returned

#### Scenario: Default TTL is 30 seconds
- **WHEN** `NewPodCache` is called with ttl <= 0
- **THEN** it SHALL use a default TTL of 30 seconds

#### Scenario: Concurrent access is safe
- **WHEN** `Get`, `Set`, `Delete`, and `Invalidate` are called concurrently from multiple goroutines
- **THEN** all operations SHALL be serialized via the RWMutex without data races

#### Scenario: Set overwrites existing entries
- **WHEN** `Set` is called for a session ID that already has a cached entry
- **THEN** the entry SHALL be replaced with the new podIP, podName, and a fresh TTL
