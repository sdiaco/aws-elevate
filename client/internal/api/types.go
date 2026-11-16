package api

import (
	"encoding/json"
	"time"
)

// Types of the api responses, one per shape of contract/api. A pointer marks a field the
// service may send as null.

type User struct {
	UserID   string `json:"userId"`
	UserName string `json:"userName"`
}

type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PermissionSet struct {
	Arn  string `json:"arn"`
	Name string `json:"name"`
}

type Me struct {
	User         User     `json:"user"`
	Roles        []string `json:"roles"`
	InfraVersion string   `json:"infraVersion"`
	MinClient    string   `json:"minClient"`
	// Only for admins: the resources the admin commands use with their own credentials.
	Installation *Installation `json:"installation"`
}

type Installation struct {
	StackID            string `json:"stackId"`
	RulesApplicationID string `json:"rulesApplicationId"`
	RulesProfileID     string `json:"rulesProfileId"`
	AlertsTopicARN     string `json:"alertsTopicArn"`
	ApprovalsTopicARN  string `json:"approvalsTopicArn"`
}

type Grant struct {
	ID              string        `json:"id"`
	State           string        `json:"state"`
	CloseReason     *string       `json:"closeReason"`
	RuleID          string        `json:"ruleId"`
	Approval        string        `json:"approval"`
	ConfigVersion   int           `json:"configVersion"`
	Requester       User          `json:"requester"`
	Approver        *User         `json:"approver"`
	ApproverGroup   *Ref          `json:"approverGroup"`
	SourcePs        PermissionSet `json:"sourcePs"`
	Account         string        `json:"account"`
	DurationSeconds int           `json:"durationSeconds"`
	Reason          string        `json:"reason"`
	Privileged      bool          `json:"privileged"`
	CreatedAt       time.Time     `json:"createdAt"`
	ActivatedAt     *time.Time    `json:"activatedAt"`
	ExpiresAt       *time.Time    `json:"expiresAt"`
	ClosedAt        *time.Time    `json:"closedAt"`
	RevokedBy       *User         `json:"revokedBy"`
	RevokeReason    *string       `json:"revokeReason"`
	Error           *string       `json:"error"`
	RuleRemoved     bool          `json:"ruleRemoved"`
	// Actions the caller may take on this grant, decided by the service.
	Actions []string `json:"actions"`
	// When a pending request times out; null in every other state.
	ApprovalExpiresAt *time.Time `json:"approvalExpiresAt"`
}

type GrantResponse struct {
	Grant Grant `json:"grant"`
}

type TimelineEntry struct {
	At     time.Time `json:"at"`
	State  string    `json:"state"`
	Reason *string   `json:"reason"`
}

type GrantDetail struct {
	Grant    Grant           `json:"grant"`
	Timeline []TimelineEntry `json:"timeline"`
}

type GrantList struct {
	Items  []Grant `json:"items"`
	Cursor *string `json:"cursor"`
}

type CreateRequest struct {
	RuleID  string `json:"ruleId"`
	Account string `json:"account"`
	// Zero leaves the duration to the service, which applies the rule default.
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	Reason          string `json:"reason"`
}

type revokeRequest struct {
	Reason string `json:"reason"`
}

// ListQuery filters GET /grants; empty fields are not sent.
type ListQuery struct {
	Scope  string
	Open   bool
	User   string
	From   string
	To     string
	Cursor string
}

type Duration struct {
	DefaultSeconds int `json:"defaultSeconds"`
	MaxSeconds     int `json:"maxSeconds"`
}

type Rule struct {
	ID            string        `json:"id"`
	Group         Ref           `json:"group"`
	PermissionSet PermissionSet `json:"permissionSet"`
	Accounts      []Ref         `json:"accounts"`
	Duration      Duration      `json:"duration"`
	Approval      string        `json:"approval"`
	ApproverGroup *Ref          `json:"approverGroup"`
	Privileged    bool          `json:"privileged"`
}

type RuleList struct {
	ConfigVersion int    `json:"configVersion"`
	Rules         []Rule `json:"rules"`
}

type PolicyRef struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Boundary struct {
	ManagedPolicyArn      *string    `json:"managedPolicyArn,omitempty"`
	CustomerManagedPolicy *PolicyRef `json:"customerManagedPolicy,omitempty"`
}

type StatementSummary struct {
	Effect     string          `json:"effect"`
	Actions    []string        `json:"actions"`
	Resources  []string        `json:"resources"`
	Conditions json.RawMessage `json:"conditions"`
}

// Permissions is "what you will get" for a rule (design §6.1).
type Permissions struct {
	Managed         []string           `json:"managed"`
	CustomerManaged []PolicyRef        `json:"customerManaged"`
	Boundary        *Boundary          `json:"boundary"`
	Inline          json.RawMessage    `json:"inline"`
	InlineSummary   []StatementSummary `json:"inlineSummary"`
}

type CheckError struct {
	RuleID  *string `json:"ruleId"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
}

type CheckResult struct {
	Valid  bool         `json:"valid"`
	Errors []CheckError `json:"errors"`
}

type VersionInfo struct {
	Number    int        `json:"number"`
	Valid     bool       `json:"valid"`
	Errors    []string   `json:"errors"`
	Author    *string    `json:"author"`
	CreatedAt *time.Time `json:"createdAt"`
	Verified  bool       `json:"verified"`
}

type ConfigVersions struct {
	// The version in use, the latest valid one; nil when none is valid.
	Current  *int          `json:"current"`
	Versions []VersionInfo `json:"versions"`
}

type ConfigVersion struct {
	Version VersionInfo `json:"version"`
	// The rules document as published; the client shows it, it does not interpret it.
	Doc json.RawMessage `json:"doc"`
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}
