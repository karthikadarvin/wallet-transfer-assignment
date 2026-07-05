# Design Notes — Wallet Transfer Service

## Schema Design

Four tables: `wallets`, `transfers`, `ledger_entries`, `idempotency_records`.

- **wallets**: stores a denormalized `balance` column (BIGINT, minor units/cents)
  rather than deriving it from the ledger on every read. This gives a single
  row to lock via `SELECT ... FOR UPDATE`, which is the mechanism used for
  concurrency safety (see below). A `CHECK (balance >= 0)` constraint is a
  DB-level backstop against negative balances even if application logic has a bug.

- **transfers**: the transfer request as a first-class entity with its own
  lifecycle (`PENDING → PROCESSED` / `PENDING → FAILED`), modeled as a Postgres
  ENUM so invalid states are impossible at the DB layer. A
  `CHECK (from_wallet_id != to_wallet_id)` prevents self-transfers.

- **ledger_entries**: immutable, append-only double-entry record. Every
  transfer produces exactly one DEBIT and one CREDIT row, written in a single
  `INSERT` (`LedgerRepository.InsertPair`) so the pair can never be created
  separately. No `updated_at` column — intentional, since these rows are
  never modified after insert.

- **idempotency_records**: `idempotency_key` is the primary key itself (not a
  separate surrogate ID), since the uniqueness of that key is the entire
  mechanism. Stores a `request_fingerprint` (SHA-256 hash of
  fromWalletId+toWalletId+amount) to detect the same key being reused with a
  different payload, and a `response_snapshot` (JSONB) so replays return the
  exact original response without recomputation.

## Idempotency Strategy

1. On each request, compute a fingerprint of the meaningful request fields.
2. Attempt to `INSERT` an `idempotency_records` row as `IN_PROGRESS` inside
   the same DB transaction as the rest of the transfer. The **unique
   constraint on `idempotency_key`** is the actual deduplication mechanism —
   not an application-level "check then insert," which has its own race
   condition under concurrent duplicate submission.
3. If the insert succeeds, proceed with the transfer.
4. If the insert fails on a unique violation, look up the existing record:
   - `COMPLETED` → return the cached `response_snapshot` (byte-identical replay).
   - `IN_PROGRESS` → a genuine concurrent duplicate is mid-flight; return a
     409 conflict rather than blocking or retrying.
   - Fingerprint mismatch on the same key → return a distinct conflict error,
     since this indicates key reuse across different requests (a client bug).
5. A transfer that resolves to `FAILED` (e.g. insufficient balance) is still
   a **resolved, idempotent outcome** — the idempotency record is marked
   `COMPLETED` pointing at the failed transfer, so retries return the same
   failure rather than re-attempting.

## Concurrency Strategy

- Both wallets involved in a transfer are locked with `SELECT ... FOR UPDATE`
  inside the transaction, **in a consistent order** (sorted by wallet ID)
  regardless of which is the source/destination. This prevents deadlocks when
  two transfers move money between the same pair of wallets in opposite
  directions.
- Balance checks and updates happen only after the lock is acquired, so two
  concurrent transfers debiting the same wallet serialize on that row rather
  than racing on a stale read.
- Verified with two tests: concurrent transfers within available balance
  (all succeed, final balance is exact) and concurrent transfers collectively
  exceeding balance (some succeed, some fail cleanly, balance never goes
  negative, no lost updates).

## Transaction Boundaries

One DB transaction per `POST /transfers` call, opened and committed in the
service layer (`TransferService.ExecuteTransfer`), spanning: idempotency
reservation, wallet locking, balance validation, transfer creation, balance
updates, ledger entry writes, transfer status update, and idempotency
completion. Any failure at any step rolls back the entire transaction —
there is no code path that leaves a partial transfer state.

## Assumptions and Tradeoffs

- Amounts are integers in minor units (cents) to avoid floating-point
  rounding errors; not explicitly stated in the assignment but a standard
  practice for money.
- `updated_at` columns are set explicitly in application code on each
  UPDATE rather than via a DB trigger — simpler, one less moving piece, at
  the cost of relying on every write path remembering to set it.
- No wallet-creation API; two wallets are seeded directly via SQL, per the
  assignment's explicit allowance to skip this.
- The optional GET endpoints (wallet balance, transfer history) were
  [included/skipped — update based on what you build] in favor of prioritizing
  correctness of the core transfer flow.
- Reconciliation between stored balance and ledger sum (a periodic consistency
  check) was considered but not implemented, given the 3–5 hour scope — noted
  here as a natural next step for a production system.

## Testing

- Service-layer tests run against a real Postgres test database (not mocks),
  since the concurrency guarantees can't be meaningfully verified against
  fakes.
- Covers: successful transfer + ledger balance invariant, idempotent replay
  (no duplicate transfer row, balance debited once), idempotency key reuse
  with a different payload (conflict), insufficient balance → FAILED with a
  correct retry behavior, self-transfer rejection, and two concurrency
  scenarios (within-balance and exceeding-balance).