# Contributing to Antigravity Gateway (`agy-gateway`)

Thank you for your interest in contributing to **Antigravity Gateway**. This project develops high-throughput, low-latency API proxy gateways bridging standard client protocols (OpenAI `/v1/chat/completions` and Anthropic `/v1/messages`) with the Google Antigravity Agent Client Protocol (ACP) daemon runtime.

As a research-driven, high-assurance systems project, we prioritize **Zero-Semantic-Loss**, thread safety, memory efficiency, zero-hardcoding, and verifiable empirical truth over speculative implementations.

---

## 1. Architectural Principles & Invariants

Every contribution must strictly adhere to the following invariants:

1. **Zero-Semantic-Loss Protocol:** No tool schemas, parameters, streamed content deltas, or system directives may be altered, truncated, or dropped during serialization/deserialization.
2. **Zero-Hardcoding & 12-Factor Configuration:** All configuration options (ports, timeouts, pool capacities, retry thresholds, model aliases) must be validated via strongly typed configuration structs backed by fail-fast environment variables.
3. **Concurrency & Thread Safety:** Shared mutable state must be guarded by synchronization primitives (`sync.Mutex`, `sync.RWMutex`, or atomic counters). Uncontrolled goroutine spawning or unbounded worker pools are strictly prohibited.
4. **Lifecycle & Resource Teardown:** Every network connection, child process handle, streaming context, file descriptor, and periodic ticker must have an explicit, automated termination and cleanup path.
5. **Deterministic Testing:** Bug fixes and new features must be accompanied by reproducible automated tests executed with the Go race detector (`-race`).

---

## 2. Development Environment Setup

### Prerequisites
- **Go Toolchain:** Go 1.25+ (LTS)
- **Container Engine:** Docker with Docker Compose v2+
- **Python:** Python 3.10+ (for integration and stress testing suites)
- **Node.js:** Node.js 18+ (for upstream CLI authentication tooling)

### Local Build & Verification
```bash
# Clone the repository
git clone https://github.com/fikrilal/agy-gateway.git
cd agy-gateway

# Download module dependencies
go mod download
go mod verify

# Compile gateway and administrative CLI binaries
go build -o bin/agy-gateway ./cmd/agy-gateway
go build -o bin/agy-accounts ./cmd/agy-accounts

# Execute complete test suite with race detector
go test -v -race ./...
```

---

## 3. Contribution Workflow

### 3.1 Issue Identification & Discussion
- Before submitting non-trivial pull requests, open an issue detailing the architectural rationale, technical scope, and reproduction steps or benchmarks.
- Ground proposals in verifiable evidence (profiler traces, Wireshark/CDP captures, or reproducible test scripts).

### 3.2 Branching & Development
- Branch naming convention:
  - `feat/short-description` for new features or capabilities.
  - `fix/short-description` for bug fixes and protocol corrections.
  - `perf/short-description` for algorithmic optimizations and latency improvements.
  - `test/short-description` for test harness expansions.

### 3.3 Semantic Documentation Standards
All exported functions, types, public methods, and non-trivial algorithms must be exhaustively annotated using standardized docstrings including:
- `@purpose`: Precise system-level objective and functional responsibility.
- `@rationale`: Design decision explaining why this pattern/algorithm was chosen.
- `@preconditions`: Required state invariants prior to invocation.
- `@param`: Parameter domain limits, valid types, and boundary conditions.
- `@returns`: Return semantics and output invariants.
- `@invariants`: Computational complexity (time/space Big-O) and thread-safety guarantees.

---

## 4. Verification & Testing Gates

Prior to submitting a Pull Request, your changes must pass the following verification checklist:

```bash
# 1. Typecheck and linting
golangci-lint run ./...

# 2. Race-detected unit tests across all packages
go test -v -race ./...

# 3. Build Docker container image locally
docker compose build agy-gateway

# 4. Run end-to-end multi-adapter stress test suite
python3 tests/e2e_stress_test.py
```

All checklist criteria must evaluate to `PASS`:
- [x] Zero compilation or linter errors.
- [x] 100% unit and integration test pass rate under `-race`.
- [x] Zero memory leaks, zero detached goroutines, and zero unhandled errors.
- [x] Clean Docker container logs with zero unmarshaling errors or uncaught panics.
- [x] `CHANGELOG.md` updated adhering to Keep a Changelog standards.

---

## 5. Pull Request Submission

1. Submit Pull Requests targeting the `main` branch.
2. Provide a clear summary of changes, problem statement, root cause analysis, and reproduction/verification commands.
3. Be prepared to engage in constructive peer review focused on correctness, memory safety, and protocol fidelity.
