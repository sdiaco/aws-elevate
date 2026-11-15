package api

// States, close reasons and error codes: the exact strings of contract/vocabulary.json,
// in the same order (a test compares them).

const (
	StatePending      = "pending"
	StateActivating   = "activating"
	StateActive       = "active"
	StateRevoking     = "revoking"
	StateRevokeFailed = "revoke_failed"
	StateClosed       = "closed"
)

var States = []string{StatePending, StateActivating, StateActive, StateRevoking, StateRevokeFailed, StateClosed}

var CloseReasons = []string{"expired", "revoked", "rejected", "timed_out", "cancelled", "invalidated", "failed"}

const (
	CodeForbidden         = "forbidden"
	CodeNotFound          = "not_found"
	CodeInvalidRequest    = "invalid_request"
	CodeDuplicate         = "duplicate"
	CodeRateLimited       = "rate_limited"
	CodeStateConflict     = "state_conflict"
	CodeActivating        = "activating"
	CodeNotReady          = "not_ready"
	CodeClientTooOld      = "client_too_old"
	CodeConfigUnavailable = "config_unavailable"
	CodeInternal          = "internal"
)

var ErrorCodes = []string{
	CodeForbidden, CodeNotFound, CodeInvalidRequest, CodeDuplicate, CodeRateLimited,
	CodeStateConflict, CodeActivating, CodeNotReady, CodeClientTooOld, CodeConfigUnavailable,
	CodeInternal,
}
