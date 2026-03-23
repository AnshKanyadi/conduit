// Package consensus implements distributed coordination using etcd.
//
// In a multi-node Conduit deployment, each session is "owned" by exactly one
// node at a time. This package uses etcd leases for session ownership,
// leader election per session, and a distributed hash table mapping session
// IDs to node addresses.
//
// If the owning node dies, etcd's lease expiry triggers failover: a replica
// node acquires the lease and takes over the session within one election
// timeout (~150–300 ms by default).
package consensus
