module github.com/agentgate/agentgate

go 1.24.7

// The dependency set is deliberately small and is vendored, so the build is
// reproducible offline and every line of third-party code in it can be read by
// a security reviewer. See docs/adr/0014-dependency-policy.md for what that
// costs and when the decision should be revisited.
require (
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/google/uuid v1.6.0
	github.com/redis/go-redis/v9 v9.7.0
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
)
